package monitoringcontroller

import (
	"context"
	"fmt"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	applyappsv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	applymetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"time"
)

const (
	monitoringEnabledConfigmapName = "enable-monitoring-configmap"
)

// MonitoringController brings back the etcd-health-monitor sidecar that was removed with https://github.com/openshift/cluster-etcd-operator/pull/873
// The sidecar itself created unnecessarily high CPU usage due to TLS handshakes (1-2 cores of overhead) and unreliable output under load.
// This controller only creates an equivalent daemon set when a configmap was created using:
// > kubectl create cm -n openshift-etcd-operator enable-monitoring-configmap
// That DS should only be used for our CI clusters to correlate failures in apiserver-related components, no removal routine is implemented for this DS.
// This is not meant for actual end users of the product and should not be used to monitor etcd.
type MonitoringController struct {
	operatorClient  v1helpers.OperatorClient
	configmapLister corev1listers.ConfigMapLister
	kubeClient      *kubernetes.Clientset
	operatorImage   string
	envVarGetter    etcdenvvar.EnvVar
}

func NewMonitoringController(operatorClient v1helpers.OperatorClient,
	eventRecorder events.Recorder,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	kubeClient *kubernetes.Clientset,
	envVarGetter etcdenvvar.EnvVar,
	operatorImage string) factory.Controller {
	c := &MonitoringController{
		operatorClient:  operatorClient,
		configmapLister: kubeInformers.ConfigMapLister(),
		kubeClient:      kubeClient,
		operatorImage:   operatorImage,
		envVarGetter:    envVarGetter,
	}

	return factory.New().WithInformers(
		operatorClient.Informer(),
		kubeInformers.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps().Informer()).
		WithSync(c.sync).ToController("MonitoringController", eventRecorder.WithComponentSuffix("monitoring-controller"))
}

func (c *MonitoringController) sync(ctx context.Context, _ factory.SyncContext) error {
	enabled, err := c.checkMonitoringEnabled()
	if err != nil {
		return err
	}

	if !enabled {
		return nil
	}

	return c.reconcileMonitoringDaemonSet(ctx)
}

func (c *MonitoringController) checkMonitoringEnabled() (bool, error) {
	enableConfigMap, err := c.configmapLister.ConfigMaps(operatorclient.OperatorNamespace).Get(monitoringEnabledConfigmapName)
	if err != nil && !k8serror.IsNotFound(err) {
		return false, fmt.Errorf("failed to retrieve configmap %s/%s: %w", operatorclient.OperatorNamespace, monitoringEnabledConfigmapName, err)
	}

	if enableConfigMap != nil {
		klog.V(4).Infof("Defrag controller enabled manually via configmap: %s/%s", operatorclient.OperatorNamespace, monitoringEnabledConfigmapName)
		return true, nil
	}

	return false, nil
}

func (c *MonitoringController) reconcileMonitoringDaemonSet(ctx context.Context) error {
	labels := map[string]string{"name": "etcd-monitoring-daemon"}

	_, _, resourceVersion, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}

	envVars := []*applycorev1.EnvVarApplyConfiguration{
		applycorev1.EnvVar().WithName("OPENSHIFT_PROFILE").WithValue("web"),
		applycorev1.EnvVar().WithName("ETCD_STATIC_POD_VERSION").WithValue(resourceVersion),
		applycorev1.EnvVar().WithName("POD_NAME").WithValueFrom(
			applycorev1.EnvVarSource().WithFieldRef(applycorev1.ObjectFieldSelector().WithFieldPath("metadata.name"))),
		applycorev1.EnvVar().WithName("NODE_NAME").WithValueFrom(
			applycorev1.EnvVarSource().WithFieldRef(applycorev1.ObjectFieldSelector().WithFieldPath("spec.nodeName"))),
	}

	for k, v := range c.envVarGetter.GetEnvVars() {
		envVars = append(envVars, applycorev1.EnvVar().WithName(k).WithValue(v))
	}

	podSpec := applycorev1.PodSpec()
	podSpec.Containers = []applycorev1.ContainerApplyConfiguration{
		*applycorev1.Container().
			WithName("eads-tracing-daemon").
			WithSecurityContext(applycorev1.SecurityContext().WithPrivileged(true)).
			WithImage(c.operatorImage).
			WithEnv(envVars...).
			WithVolumeMounts(
				applycorev1.VolumeMount().WithName("cert-dir").WithMountPath("/etc/kubernetes/static-pod-certs"),
				applycorev1.VolumeMount().WithName("log-dir").WithMountPath("/var/log/etcd/"),
			).
			WithCommand("cluster-etcd-operator", "monitor").
			WithArgs("--targets=$(ETCDCTL_ENDPOINTS)",
				"--probe-interval=1s",
				"--log-outputs=stderr",
				"--log-outputs=/var/log/etcd/etcd-health-probe.log",
				"--enable-log-rotation",
				"--pod-name=$(POD_NAME)",
				"--static-pod-version=$(ETCD_STATIC_POD_VERSION)",
				"--cert-file=$(ETCDCTL_CERT)",
				"--key-file=$(ETCDCTL_KEY)",
				"--cacert-file=$(ETCDCTL_CACERT)"),
	}
	podSpec.NodeSelector = map[string]string{"node-role.kubernetes.io/master": ""}
	podSpec.Tolerations = []applycorev1.TolerationApplyConfiguration{
		*applycorev1.Toleration().WithKey("node-role.kubernetes.io/master").WithOperator(corev1.TolerationOpExists).WithEffect(corev1.TaintEffectNoSchedule),
	}

	podSpec.Volumes = []applycorev1.VolumeApplyConfiguration{
		*applycorev1.Volume().WithName("cert-dir").WithHostPath(applycorev1.HostPathVolumeSource().WithPath("/etc/kubernetes/static-pod-resources/etcd-certs")),
		*applycorev1.Volume().WithName("log-dir").WithHostPath(applycorev1.HostPathVolumeSource().WithPath("/var/log/etcd")),
	}

	ds := applyappsv1.DaemonSet("monitoring-daemon", operatorclient.TargetNamespace).WithSpec(applyappsv1.DaemonSetSpec().WithTemplate(
		applycorev1.PodTemplateSpec().WithName("monitoring-daemon").WithSpec(podSpec).WithLabels(labels),
	).WithSelector(applymetav1.LabelSelector().WithMatchLabels(labels)).
		WithUpdateStrategy(applyappsv1.DaemonSetUpdateStrategy().WithRollingUpdate(applyappsv1.RollingUpdateDaemonSet().WithMaxUnavailable(intstr.FromInt(3)))))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = c.kubeClient.AppsV1().DaemonSets(operatorclient.TargetNamespace).Apply(ctx, ds, metav1.ApplyOptions{FieldManager: "monitoring-daemon"})
	if err != nil {
		return err
	}
	return nil
}
