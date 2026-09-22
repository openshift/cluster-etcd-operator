package operator

import (
	"testing"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestTnfWorkloadTemplatesUseReadOnlyRootFilesystem(t *testing.T) {
	t.Run("pacemaker status collector CronJob", func(t *testing.T) {
		cronJob := jobs.ReadCronJobV1OrDie(bindata.MustAsset("tnfdeployment/cronjob.yaml"))
		podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec

		require.Len(t, podSpec.Containers, 1)
		container := podSpec.Containers[0]
		require.Equal(t, "collector", container.Name)
		requireReadOnlyRootFilesystem(t, podSpec.Containers)
		requireReadOnlyRootFilesystem(t, podSpec.InitContainers)
		require.NotNil(t, container.SecurityContext.Privileged)
		require.NotNil(t, container.SecurityContext.AllowPrivilegeEscalation)
		require.True(t, *container.SecurityContext.Privileged)
		require.True(t, *container.SecurityContext.AllowPrivilegeEscalation)
		require.True(t, podSpec.HostPID)
		require.Equal(t, []string{"<injected>"}, container.Command)
		require.Equal(t, resource.MustParse("10m"), container.Resources.Requests[corev1.ResourceCPU])
		require.Equal(t, resource.MustParse("32Mi"), container.Resources.Requests[corev1.ResourceMemory])
		require.Equal(t, "tnf-setup-manager", podSpec.ServiceAccountName)
		require.Equal(t, map[string]string{
			"app":                    "tnf-job",
			"app.kubernetes.io/name": "<injected>",
		}, cronJob.Spec.JobTemplate.Spec.Template.Labels)
		require.Equal(t, map[string]string{"node-role.kubernetes.io/master": ""}, podSpec.NodeSelector)
		require.NotNil(t, podSpec.Affinity)
		require.NotNil(t, podSpec.Affinity.NodeAffinity)
		require.Len(t, podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms, 1)
		require.Equal(t, []corev1.NodeSelectorRequirement{
			{Key: "node.kubernetes.io/not-ready", Operator: corev1.NodeSelectorOpDoesNotExist},
			{Key: "node.kubernetes.io/unreachable", Operator: corev1.NodeSelectorOpDoesNotExist},
		}, podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions)
		require.Equal(t, expectedTolerations(), podSpec.Tolerations)
		require.Empty(t, container.VolumeMounts)
		require.Empty(t, podSpec.Volumes)
	})

	t.Run("cert watcher DaemonSet", func(t *testing.T) {
		daemonSet := buildCertWatcherDaemonSet()
		podSpec := daemonSet.Spec.Template.Spec

		require.Equal(t, certWatcherName, daemonSet.Name)
		require.Len(t, podSpec.Containers, 1)
		container := podSpec.Containers[0]
		require.Equal(t, "cert-watcher", container.Name)
		requireReadOnlyRootFilesystem(t, podSpec.Containers)
		requireReadOnlyRootFilesystem(t, podSpec.InitContainers)
		require.NotNil(t, container.SecurityContext.Privileged)
		require.NotNil(t, container.SecurityContext.AllowPrivilegeEscalation)
		require.True(t, *container.SecurityContext.Privileged)
		require.True(t, *container.SecurityContext.AllowPrivilegeEscalation)
		require.True(t, podSpec.HostPID)
		require.Equal(t, []string{"tnf-monitor", "watch-certs", "--cert-dir=/certs/configmaps/etcd-all-bundles"}, container.Command)
		require.Equal(t, resource.MustParse("10m"), container.Resources.Requests[corev1.ResourceCPU])
		require.Equal(t, resource.MustParse("32Mi"), container.Resources.Requests[corev1.ResourceMemory])
		require.Equal(t, "tnf-setup-manager", podSpec.ServiceAccountName)
		require.Equal(t, map[string]string{"app.kubernetes.io/name": certWatcherName}, daemonSet.Spec.Template.Labels)
		require.Equal(t, map[string]string{"node-role.kubernetes.io/master": ""}, podSpec.NodeSelector)
		require.Nil(t, podSpec.Affinity)
		require.Equal(t, expectedTolerations(), podSpec.Tolerations)
		require.Equal(t, []corev1.VolumeMount{{Name: "etcd-certs", MountPath: "/certs", ReadOnly: true}}, container.VolumeMounts)
		require.Len(t, podSpec.Volumes, 1)
		require.Equal(t, "etcd-certs", podSpec.Volumes[0].Name)
		require.NotNil(t, podSpec.Volumes[0].HostPath)
		require.Equal(t, certHostPath, podSpec.Volumes[0].HostPath.Path)
		require.NotNil(t, podSpec.Volumes[0].HostPath.Type)
		require.Equal(t, corev1.HostPathDirectory, *podSpec.Volumes[0].HostPath.Type)
	})
}

func expectedTolerations() []corev1.Toleration {
	return []corev1.Toleration{
		{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/memory-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/disk-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/pid-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
}

func requireReadOnlyRootFilesystem(t *testing.T, containers []corev1.Container) {
	t.Helper()

	for _, container := range containers {
		require.NotNil(t, container.SecurityContext, "container %q must define a security context", container.Name)
		require.NotNil(t, container.SecurityContext.ReadOnlyRootFilesystem, "container %q must explicitly set readOnlyRootFilesystem", container.Name)
		require.True(t, *container.SecurityContext.ReadOnlyRootFilesystem, "container %q must set readOnlyRootFilesystem to true", container.Name)
	}
}
