package render

import (
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

func TestCertSingleNode(t *testing.T) {
	enabled, disabled := sets.Set[configv1.FeatureGateName]{}, sets.Set[configv1.FeatureGateName]{}
	disabled.Insert(features.FeatureShortCertRotation)
	node := u.FakeNode("cp-1", u.WithMasterLabel(), u.WithNodeInternalIP("192.168.2.1"))
	secrets, bundles, err := createCertSecrets([]*corev1.Node{node}, enabled, disabled)
	require.NoError(t, err)

	require.Equal(t, 8, len(secrets))
	require.Equal(t, 3, len(bundles))

	u.AssertCertificateCorrectness(t, secrets)
	u.AssertBundleCorrectness(t, secrets, bundles)
	u.AssertNodeSpecificCertificates(t, node, secrets)
}

func TestCertsMultiNode(t *testing.T) {
	enabled, disabled := sets.Set[configv1.FeatureGateName]{}, sets.Set[configv1.FeatureGateName]{}
	disabled.Insert(features.FeatureShortCertRotation)
	nodes := []*corev1.Node{
		u.FakeNode("cp-1", u.WithMasterLabel(), u.WithNodeInternalIP("192.168.2.1")),
		u.FakeNode("cp-2", u.WithMasterLabel(), u.WithNodeInternalIP("192.168.2.2")),
		u.FakeNode("cp-3", u.WithMasterLabel(), u.WithNodeInternalIP("192.168.2.3")),
	}
	secrets, bundles, err := createCertSecrets(nodes, enabled, disabled)
	require.NoError(t, err)

	require.Equal(t, 14, len(secrets))
	require.Equal(t, 3, len(bundles))

	u.AssertCertificateCorrectness(t, secrets)
	u.AssertBundleCorrectness(t, secrets, bundles)
	for _, node := range nodes {
		u.AssertNodeSpecificCertificates(t, node, secrets)
	}
}
