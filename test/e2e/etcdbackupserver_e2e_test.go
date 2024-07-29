package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	configv1alpha1 "github.com/openshift/api/config/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/openshift/library-go/test/library"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"

	"github.com/stretchr/testify/require"
)

const (
	defaultBackupCRName = "default"
	testSchedule        = "*/5 * * * *"
	testTimeZone        = "UTC"
	backupHostPath      = "/var/backup/etcd"
)

func TestEtcdBackupServer(t *testing.T) {
	configClient := framework.NewConfigClient(t)
	clientSet := framework.NewClientSet("")
	etcdPodsClient := clientSet.CoreV1Interface.Pods(operandNameSpace)

	backupCR := createDefaultBackupCR(testSchedule, testTimeZone)
	_, err := configClient.Backups().Create(context.Background(), backupCR, metav1.CreateOptions{})
	require.NoError(t, err)

	err = library.WaitForPodsToStabilizeOnTheSameRevision(t, etcdPodsClient, etcdLabelSelector, 5, 1*time.Minute, 5*time.Second, 30*time.Minute)
	require.NoError(t, err)

	masterNodes, err := clientSet.CoreV1Interface.Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: masterNodeLabel})
	require.NoErrorf(t, err, "error listing master nodes")

	for _, node := range masterNodes.Items {
		err = isBackupExist(t, node)
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		cErr := configClient.Backups().Delete(context.Background(), backupCR.Name, metav1.DeleteOptions{})
		require.NoError(t, cErr)
	})
}

func createDefaultBackupCR(schedule, timeZone string) *configv1alpha1.Backup {
	return &configv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name: defaultBackupCRName,
			Annotations: map[string]string{
				"default": "true",
			},
		},
		Spec: configv1alpha1.BackupSpec{
			EtcdBackupSpec: configv1alpha1.EtcdBackupSpec{
				Schedule: schedule,
				TimeZone: timeZone,
				RetentionPolicy: configv1alpha1.RetentionPolicy{
					RetentionType: configv1alpha1.RetentionTypeNumber,
				},
			},
		},
	}
}

func isBackupExist(t *testing.T, node corev1.Node) error {
	client := framework.NewCoreClient(t)
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.SimpleNameGenerator.GenerateName("backup-finder-pod" + "-"),
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: corev1.PodSpec{
			NodeName: node.Name,
			Volumes: []corev1.Volume{
				{
					Name: "backup-dir",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: backupHostPath,
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:       "finder",
					Image:      ShellImage,
					Command:    []string{"find", "."},
					WorkingDir: backupHostPath,
					Resources:  corev1.ResourceRequirements{},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "backup-dir",
							ReadOnly:  false,
							MountPath: backupHostPath,
						},
					},
					SecurityContext: &corev1.SecurityContext{Privileged: pointer.Bool(true)},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	_, err := client.Pods(OpenShiftEtcdNamespace).Create(context.Background(), &pod, metav1.CreateOptions{})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, 10*time.Second, false, func(ctx context.Context) (done bool, err error) {
		p, err := client.Pods(OpenShiftEtcdNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			klog.Infof("error while getting finder pod: %v", err)
			return false, nil
		}

		if p.Status.Phase == corev1.PodFailed {
			return true, fmt.Errorf("finder pod failed with status: %s", p.Status.String())
		}

		return p.Status.Phase == corev1.PodSucceeded, nil
	})
	require.NoErrorf(t, err, "waiting for finder pod error")

	logBytes, err := client.Pods(OpenShiftEtcdNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Do(ctx).Raw()
	require.NoError(t, err)

	files := strings.Split(string(logBytes), "\n")
	klog.Infof("found files on node [%s]: %v", node.Name, files)

	require.Contains(t, files[2], "snapshot_")
	require.Contains(t, files[3], "static_kuberesources_")

	return nil
}
