package installercontroller

import (
	operatorv1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
)

func MutateInstallerPod(pod *corev1.Pod, nodeName string, operatorSpec *operatorv1.StaticPodOperatorSpec, revision int32) error {
	pod.Spec.Containers[0].Args = append(
		pod.Spec.Containers[0].Args,
		"--pod-args=7500",
	)

	return nil
}
