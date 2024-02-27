package etcdcertcleaner

import (
	"context"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"testing"
	"time"
)

func TestPreDeleteLabelHappyPath(t *testing.T) {
	cleaner := newMockCleaner(t, []runtime.Object{
		u.FakeNode("master-0", u.WithMasterLabel()),
		u.FakeSecret(
			operatorclient.TargetNamespace,
			tlshelpers.GetPeerClientSecretNameForNode("master-1"),
			map[string][]byte{}),
	})
	err := cleaner.sync(context.Background(), nil)
	require.NoError(t, err)

	// secret should NOT be deleted
	updated := false
	deleted := false
	fx := cleaner.kubeClient.(*fake.Clientset)
	for _, action := range fx.Actions() {
		if action.Matches("delete", "secrets") {
			require.Equal(t, "etcd-peer-master-1", action.(k8stesting.DeleteAction).GetName())
			deleted = true
		} else if action.Matches("update", "secrets") {
			// secret should be relabeled with the grace annotation though
			obj := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
			require.Equal(t, "etcd-peer-master-1", obj.Name)
			require.Contains(t, obj.Annotations, DeletionGracePeriodAnnotation)
			updated = true
		}
	}

	require.Truef(t, updated, "expected secret to be updated, but only found actions: %v", fx.Actions())
	require.Falsef(t, deleted, "expected secret to not be deleted, but only found actions: %v", fx.Actions())
}

func TestDeleteHappyPath(t *testing.T) {
	cleaner := newMockCleaner(t, []runtime.Object{
		u.FakeNode("master-0", u.WithMasterLabel()),
		u.FakeSecretWithAnnotations(
			operatorclient.TargetNamespace,
			tlshelpers.GetPeerClientSecretNameForNode("master-1"),
			map[string][]byte{},
			map[string]string{DeletionGracePeriodAnnotation: time.Now().Add(DeletionGracePeriod * -2).Format(time.RFC3339)}),
	})
	err := cleaner.sync(context.Background(), nil)
	require.NoError(t, err)

	// secret should have been deleted
	deleted := false
	fx := cleaner.kubeClient.(*fake.Clientset)
	for _, action := range fx.Actions() {
		if action.Matches("delete", "secrets") {
			require.Equal(t, "etcd-peer-master-1", action.(k8stesting.DeleteAction).GetName())
			deleted = true
		}
	}

	require.Truef(t, deleted, "expected secret to be deleted, but only found actions: %v", fx.Actions())
}

func TestNodeCameBackWithinGrace(t *testing.T) {
	cleaner := newMockCleaner(t, []runtime.Object{
		u.FakeNode("master-0", u.WithMasterLabel()),
		u.FakeSecretWithAnnotations(
			operatorclient.TargetNamespace,
			tlshelpers.GetPeerClientSecretNameForNode("master-0"),
			map[string][]byte{},
			map[string]string{DeletionGracePeriodAnnotation: time.Now().Add(-DeletionGracePeriod / 2).Format(time.RFC3339)}),
	})
	err := cleaner.sync(context.Background(), nil)
	require.NoError(t, err)

	// secret should NOT be deleted
	updated := false
	deleted := false
	fx := cleaner.kubeClient.(*fake.Clientset)
	for _, action := range fx.Actions() {
		if action.Matches("delete", "secrets") {
			require.Equal(t, "etcd-peer-master-0", action.(k8stesting.DeleteAction).GetName())
			deleted = true
		} else if action.Matches("update", "secrets") {
			// secret should be relabeled and have the grace annotation removed again
			obj := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
			require.Equal(t, "etcd-peer-master-0", obj.Name)
			require.NotContains(t, obj.Annotations, DeletionGracePeriodAnnotation)
			updated = true
		}
	}

	require.Truef(t, updated, "expected secret to be updated, but only found actions: %v", fx.Actions())
	require.Falsef(t, deleted, "expected secret to not be deleted, but only found actions: %v", fx.Actions())
}

func TestDeleteNoDanglingSecrets(t *testing.T) {
	cleaner := newMockCleaner(t, []runtime.Object{
		u.FakeNode("master-0", u.WithMasterLabel()),
		u.FakeSecret(operatorclient.TargetNamespace, tlshelpers.GetPeerClientSecretNameForNode("master-0"), map[string][]byte{}),
		u.FakeNode("master-1", u.WithMasterLabel()),
		u.FakeSecret(operatorclient.TargetNamespace, tlshelpers.GetPeerClientSecretNameForNode("master-1"), map[string][]byte{}),
		u.FakeNode("master-2", u.WithMasterLabel()),
		u.FakeSecret(operatorclient.TargetNamespace, tlshelpers.GetPeerClientSecretNameForNode("master-2"), map[string][]byte{}),
	})
	secrets, err := cleaner.findUnusedNodeBasedSecrets(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, len(secrets))
}

func TestDeleteNoSecrets(t *testing.T) {
	cleaner := newMockCleaner(t, []runtime.Object{
		u.FakeNode("master-0", u.WithMasterLabel()),
		u.FakeNode("master-1", u.WithMasterLabel()),
		u.FakeNode("master-2", u.WithMasterLabel()),
	})
	secrets, err := cleaner.findUnusedNodeBasedSecrets(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, len(secrets))
}

func newMockCleaner(t *testing.T, objects []runtime.Object) *EtcdCertCleanerController {
	fakeKubeClient := fake.NewSimpleClientset(objects...)
	kubeInformers := v1helpers.NewKubeInformersForNamespaces(fakeKubeClient, "", operatorclient.TargetNamespace)
	nodeSelector, err := labels.Parse("node-role.kubernetes.io/master")
	require.NoError(t, err)

	cc := &EtcdCertCleanerController{
		kubeClient:         fakeKubeClient,
		secretClient:       fakeKubeClient.CoreV1(),
		secretLister:       kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister(),
		masterNodeLister:   kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		masterNodeSelector: nodeSelector,
	}

	stopChan := make(chan struct{})
	t.Cleanup(func() {
		close(stopChan)
	})

	kubeInformers.Start(stopChan)
	for ns := range kubeInformers.Namespaces() {
		kubeInformers.InformersFor(ns).WaitForCacheSync(stopChan)
	}

	return cc
}
