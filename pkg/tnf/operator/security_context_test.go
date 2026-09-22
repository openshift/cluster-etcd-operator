package operator

import (
	"testing"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestTnfWorkloadTemplatesUseReadOnlyRootFilesystem(t *testing.T) {
	t.Run("pacemaker status collector CronJob", func(t *testing.T) {
		cronJob := jobs.ReadCronJobV1OrDie(bindata.MustAsset("tnfdeployment/cronjob.yaml"))
		podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec

		require.Len(t, podSpec.Containers, 1)
		require.Equal(t, "collector", podSpec.Containers[0].Name)
		requireReadOnlyRootFilesystem(t, podSpec.Containers)
		requireReadOnlyRootFilesystem(t, podSpec.InitContainers)
		require.NotNil(t, podSpec.Containers[0].SecurityContext.Privileged)
		require.NotNil(t, podSpec.Containers[0].SecurityContext.AllowPrivilegeEscalation)
		require.True(t, *podSpec.Containers[0].SecurityContext.Privileged)
		require.True(t, *podSpec.Containers[0].SecurityContext.AllowPrivilegeEscalation)
		require.True(t, podSpec.HostPID)
	})

	t.Run("cert watcher DaemonSet", func(t *testing.T) {
		daemonSet := buildCertWatcherDaemonSet()
		podSpec := daemonSet.Spec.Template.Spec

		require.Equal(t, certWatcherName, daemonSet.Name)
		require.Len(t, podSpec.Containers, 1)
		require.Equal(t, "cert-watcher", podSpec.Containers[0].Name)
		requireReadOnlyRootFilesystem(t, podSpec.Containers)
		requireReadOnlyRootFilesystem(t, podSpec.InitContainers)
		require.NotNil(t, podSpec.Containers[0].SecurityContext.Privileged)
		require.NotNil(t, podSpec.Containers[0].SecurityContext.AllowPrivilegeEscalation)
		require.True(t, *podSpec.Containers[0].SecurityContext.Privileged)
		require.True(t, *podSpec.Containers[0].SecurityContext.AllowPrivilegeEscalation)
		require.True(t, podSpec.HostPID)
	})
}

func requireReadOnlyRootFilesystem(t *testing.T, containers []corev1.Container) {
	t.Helper()

	for _, container := range containers {
		require.NotNil(t, container.SecurityContext, "container %q must define a security context", container.Name)
		require.NotNil(t, container.SecurityContext.ReadOnlyRootFilesystem, "container %q must explicitly set readOnlyRootFilesystem", container.Name)
		require.True(t, *container.SecurityContext.ReadOnlyRootFilesystem, "container %q must set readOnlyRootFilesystem to true", container.Name)
	}
}
