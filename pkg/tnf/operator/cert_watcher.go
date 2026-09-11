package operator

import (
	"context"
	"fmt"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	certWatcherName = "tnf-cert-watcher"
	certHostPath    = "/etc/kubernetes/static-pod-resources/etcd-certs"
)

// ensureCertWatcherDaemonSet creates or updates the cert-watcher DaemonSet.
// The DaemonSet runs on each control plane node and watches CA bundle files
// on disk. When they change, it sets restart_no_leave and restarts the local
// etcd — preventing force_new_cluster during CA rotation.
func (c *pacemakerLifecycleManager) ensureCertWatcherDaemonSet(ctx context.Context) error {
	desired := buildCertWatcherDaemonSet()

	existing, err := c.kubeClient.AppsV1().DaemonSets(operatorclient.TargetNamespace).Get(ctx, certWatcherName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		klog.Infof("Creating cert-watcher DaemonSet")
		_, err = c.kubeClient.AppsV1().DaemonSets(operatorclient.TargetNamespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("failed to get cert-watcher DaemonSet: %w", err)
	}

	existing.Spec.Template = desired.Spec.Template
	klog.Infof("Updating cert-watcher DaemonSet")
	_, err = c.kubeClient.AppsV1().DaemonSets(operatorclient.TargetNamespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func buildCertWatcherDaemonSet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      certWatcherName,
			Namespace: operatorclient.TargetNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": certWatcherName,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": certWatcherName,
				},
			},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": certWatcherName,
					},
					Annotations: map[string]string{
						"openshift.io/required-scc": "privileged",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:                     "cert-watcher",
						Image:                    os.Getenv("OPERATOR_IMAGE"),
						ImagePullPolicy:          corev1.PullIfNotPresent,
						Command:                  []string{"tnf-monitor", "watch-certs", "--cert-dir=/certs/configmaps/etcd-all-bundles"},
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    *parseQuantity("10m"),
								corev1.ResourceMemory: *parseQuantity("32Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							Privileged:               ptr.To(true),
							AllowPrivilegeEscalation: ptr.To(true),
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "etcd-certs",
							MountPath: "/certs",
							ReadOnly:  true,
						}},
					}},
					HostPID:                       true,
					ServiceAccountName:            "tnf-setup-manager",
					PriorityClassName:             "system-node-critical",
					TerminationGracePeriodSeconds: ptr.To(int64(10)),
					NodeSelector: map[string]string{
						"node-role.kubernetes.io/master": "",
					},
					Tolerations: []corev1.Toleration{
						{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Key: "node.kubernetes.io/memory-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Key: "node.kubernetes.io/disk-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Key: "node.kubernetes.io/pid-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
					},
					Volumes: []corev1.Volume{{
						Name: "etcd-certs",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: certHostPath,
								Type: ptr.To(corev1.HostPathDirectory),
							},
						},
					}},
				},
			},
		},
	}
}

func parseQuantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
