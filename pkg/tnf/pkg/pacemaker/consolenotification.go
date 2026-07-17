package pacemaker

import (
	"context"
	"fmt"
	"strings"
	"time"

	consolev1 "github.com/openshift/api/console/v1"
	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	consoleNotificationName = "pacemaker-etcd-failure"

	// PatternFly danger palette for the banner.
	notificationTextColor       = "#fff"
	notificationBackgroundColor = "#c9190b"

	troubleshootingURL  = "https://docs.openshift.com/container-platform/latest/edge_computing/two-node-fencing/tnf-troubleshooting.html"
	troubleshootingText = "Troubleshooting guide"
)

var consoleNotificationGVR = schema.GroupVersionResource{
	Group:    "console.openshift.io",
	Version:  "v1",
	Resource: "consolenotifications",
}

// consoleNotificationController manages a ConsoleNotification banner that surfaces
// pacemaker health problems directly in the OpenShift web console. It watches the
// PacemakerCluster CR (via a shared informer) and creates or removes the banner
// based on the health of etcd resources and fencing agents.
//
// Uses the dynamic client because the console client-go package is not vendored.
type consoleNotificationController struct {
	dynamicClient     dynamic.Interface
	pacemakerInformer cache.SharedIndexInformer
}

// NewConsoleNotificationController creates a controller that manages a ConsoleNotification
// banner for pacemaker podman-etcd failures. The controller shares the PacemakerCluster
// informer with the healthcheck and metrics controllers.
func NewConsoleNotificationController(
	pacemakerInformer cache.SharedIndexInformer,
	dynamicClient dynamic.Interface,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &consoleNotificationController{
		dynamicClient:     dynamicClient,
		pacemakerInformer: pacemakerInformer,
	}

	return factory.New().
		ResyncEvery(HealthCheckResyncInterval).
		WithInformers(pacemakerInformer).
		WithSync(c.sync).
		ToController("ConsoleNotificationController", eventRecorder.WithComponentSuffix("console-notification"))
}

func (c *consoleNotificationController) sync(ctx context.Context, _ factory.SyncContext) error {
	klog.V(4).Infof("syncing console notification for pacemaker health")

	item, exists, err := c.pacemakerInformer.GetStore().GetByKey(PacemakerClusterResourceName)
	if err != nil {
		return err
	}

	if !exists {
		return c.deleteNotification(ctx)
	}

	cr, ok := item.(*pacmkrv1.PacemakerCluster)
	if !ok {
		return fmt.Errorf("unexpected object type in informer store: %T", item)
	}

	problems := evaluateHealth(cr)
	if len(problems) == 0 {
		return c.deleteNotification(ctx)
	}

	return c.ensureNotification(ctx, problems)
}

// evaluateHealth inspects the PacemakerCluster CR for conditions that warrant a
// console notification. Returns a list of human-readable problem descriptions,
// or nil when the cluster is healthy.
func evaluateHealth(cr *pacmkrv1.PacemakerCluster) []string {
	var problems []string

	if !cr.Status.LastUpdated.IsZero() && time.Since(cr.Status.LastUpdated.Time) > StatusStalenessThreshold {
		problems = append(problems, "Pacemaker status has not been updated recently — health monitoring may be offline")
	}

	if getConditionStatus(cr.Status.Conditions, pacmkrv1.ClusterInServiceConditionType) == metav1.ConditionFalse {
		problems = append(problems, "Pacemaker cluster is in maintenance mode")
	}

	if cr.Status.Nodes == nil {
		return problems
	}

	for i := range *cr.Status.Nodes {
		node := &(*cr.Status.Nodes)[i]
		name := node.NodeName

		if getConditionStatus(node.Conditions, pacmkrv1.NodeOnlineConditionType) == metav1.ConditionFalse {
			problems = append(problems, fmt.Sprintf("Node %s is offline", name))
			continue
		}

		for j := range node.Resources {
			res := &node.Resources[j]
			if res.Name != pacmkrv1.PacemakerClusterResourceNameEtcd {
				continue
			}
			if getConditionStatus(res.Conditions, pacmkrv1.ResourceHealthyConditionType) == metav1.ConditionFalse {
				problems = append(problems, fmt.Sprintf("Etcd resource is unhealthy on node %s", name))
			}
		}

		if getConditionStatus(node.Conditions, pacmkrv1.NodeFencingAvailableConditionType) == metav1.ConditionFalse {
			problems = append(problems, fmt.Sprintf("Fencing is unavailable on node %s — automatic recovery is not possible", name))
		}
	}

	return problems
}

func (c *consoleNotificationController) ensureNotification(ctx context.Context, problems []string) error {
	text := strings.Join(problems, ". ") + ". Check pacemaker status for details."

	notification := &consolev1.ConsoleNotification{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "console.openshift.io/v1",
			Kind:       "ConsoleNotification",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: consoleNotificationName,
			Labels: map[string]string{
				"app":                          "cluster-etcd-operator",
				"app.kubernetes.io/part-of":    "cluster-etcd-operator",
				"app.kubernetes.io/managed-by": "cluster-etcd-operator",
			},
		},
		Spec: consolev1.ConsoleNotificationSpec{
			Text:            text,
			Location:        consolev1.BannerTop,
			Color:           notificationTextColor,
			BackgroundColor: notificationBackgroundColor,
			Link: &consolev1.Link{
				Href: troubleshootingURL,
				Text: troubleshootingText,
			},
		},
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(notification)
	if err != nil {
		return fmt.Errorf("failed to convert ConsoleNotification to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}

	existing, err := c.dynamicClient.Resource(consoleNotificationGVR).Get(ctx, consoleNotificationName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := c.dynamicClient.Resource(consoleNotificationGVR).Create(ctx, u, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create ConsoleNotification: %w", err)
		}
		klog.Infof("Created console notification: %s", text)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get ConsoleNotification: %w", err)
	}

	existingText, _, _ := unstructured.NestedString(existing.Object, "spec", "text")
	if existingText == text {
		klog.V(4).Infof("Console notification already up to date")
		return nil
	}

	u.SetResourceVersion(existing.GetResourceVersion())
	if _, err := c.dynamicClient.Resource(consoleNotificationGVR).Update(ctx, u, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update ConsoleNotification: %w", err)
	}
	klog.Infof("Updated console notification: %s", text)
	return nil
}

func (c *consoleNotificationController) deleteNotification(ctx context.Context) error {
	err := c.dynamicClient.Resource(consoleNotificationGVR).Delete(ctx, consoleNotificationName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete ConsoleNotification: %w", err)
	}
	if err == nil {
		klog.Infof("Deleted console notification: pacemaker health restored")
	}
	return nil
}
