package tools

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	setupEventNamespace = "openshift-etcd"
	setupJobName        = "tnf-setup-job"
	setupSourceComponent = "tnf-setup-runner"
)

// RecordSetupEvent creates a Normal Kubernetes event in openshift-etcd for a
// TNF setup lifecycle milestone. Failures are logged but never returned —
// event recording must not block the setup.
func RecordSetupEvent(ctx context.Context, kubeClient kubernetes.Interface, reason, message string) {
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("tnf-%s-%d", reason, time.Now().UnixNano()),
			Namespace: setupEventNamespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Job",
			Name:       setupJobName,
			Namespace:  setupEventNamespace,
			APIVersion: "batch/v1",
		},
		Reason:         reason,
		Message:        message,
		Type:           corev1.EventTypeNormal,
		Source:         corev1.EventSource{Component: setupSourceComponent},
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Count:          1,
	}

	if _, err := kubeClient.CoreV1().Events(setupEventNamespace).Create(ctx, event, metav1.CreateOptions{}); err != nil {
		klog.Warningf("Failed to record %s event: %v", reason, err)
	} else {
		klog.Infof("Recorded event: %s - %s", reason, message)
	}
}
