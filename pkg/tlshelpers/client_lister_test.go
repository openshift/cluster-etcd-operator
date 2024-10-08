package tlshelpers

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPanicOnDifferentNamespaces(t *testing.T) {
	ls := &ConfigMapClientLister{Namespace: "ns"}
	require.Panics(t, func() {
		ls.ConfigMaps("other")
	})
	require.Equal(t, ls, ls.ConfigMaps("ns"))

	lsx := &SecretClientLister{Namespace: "ns"}
	require.Panics(t, func() {
		lsx.Secrets("other")
	})
	require.Equal(t, lsx, lsx.Secrets("ns"))
}

func TestConfigMaps(t *testing.T) {
	fakeObjs := []runtime.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm1"},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm2", Labels: map[string]string{"a": "b"}},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm3"},
		},
	}
	fakeKubeClient := fake.NewSimpleClientset(fakeObjs...)
	ls := &ConfigMapClientLister{ConfigMapClient: fakeKubeClient.CoreV1().ConfigMaps("ns"), Namespace: "ns"}
	list, err := ls.List(labels.Everything())
	require.NoError(t, err)
	require.Equal(t, 3, len(list))

	parsedLabel, err := labels.Parse("a=b")
	require.NoError(t, err)
	list, err = ls.List(parsedLabel)
	require.NoError(t, err)
	require.Equal(t, 1, len(list))

	get, err := ls.Get("cm1")
	require.NoError(t, err)
	require.Equal(t, "cm1", get.Name)
}

func TestSecrets(t *testing.T) {
	fakeObjs := []runtime.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm1"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm2", Labels: map[string]string{"a": "b"}},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm3"},
		},
	}
	fakeKubeClient := fake.NewSimpleClientset(fakeObjs...)
	ls := &SecretClientLister{SecretClient: fakeKubeClient.CoreV1().Secrets("ns"), Namespace: "ns"}
	list, err := ls.List(labels.Everything())
	require.NoError(t, err)
	require.Equal(t, 3, len(list))

	parsedLabel, err := labels.Parse("a=b")
	require.NoError(t, err)
	list, err = ls.List(parsedLabel)
	require.NoError(t, err)
	require.Equal(t, 1, len(list))

	get, err := ls.Get("cm1")
	require.NoError(t, err)
	require.Equal(t, "cm1", get.Name)
}
