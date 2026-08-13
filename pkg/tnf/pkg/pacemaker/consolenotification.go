package pacemaker

import (
	"context"
	"fmt"
	"slices"
	"strings"

	consolev1 "github.com/openshift/api/console/v1"
	pacmkrv1 "github.com/openshift/api/etcd/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	notificationTextColor       = "#fff"
	notificationBackgroundColor = "#c9190b"

	docsURLFormat = "https://docs.redhat.com/en/documentation/openshift_container_platform/%s/html/installing_a_two_node_openshift_cluster/two-node-with-fencing"
)

type notificationCategory struct {
	name         string
	docsFragment string
	linkText     string
}

var (
	categoryDegraded = notificationCategory{
		name:         "pacemaker-cluster-degraded",
		docsFragment: "#operating-a-degraded-tnf",
		linkText:     "Recovery guide",
	}

	categoryTroubleshooting = notificationCategory{
		name:         "pacemaker-troubleshooting",
		docsFragment: "#installing-post-tnf",
		linkText:     "Troubleshooting guide",
	}

	allCategories = []notificationCategory{categoryDegraded, categoryTroubleshooting}

	// degradedPatterns match problems routed to the degraded-operations notification.
	degradedPatterns = []string{"offline", "unhealthy", "fencing", "maintenance", "insufficient", "excessive"}

	consoleNotificationGVR = schema.GroupVersionResource{
		Group:    "console.openshift.io",
		Version:  "v1",
		Resource: "consolenotifications",
	}
)

// classifyProblems splits errors and warnings into the two notification categories.
// Problems not matching any degraded pattern are routed to troubleshooting.
func classifyProblems(status *HealthStatus) (degraded, troubleshooting []string) {
	for _, msg := range slices.Concat(status.Errors, status.Warnings) {
		if isDegradedProblem(msg) {
			degraded = append(degraded, msg)
		} else {
			troubleshooting = append(troubleshooting, msg)
		}
	}
	return
}

func isDegradedProblem(msg string) bool {
	lower := strings.ToLower(msg)
	for _, p := range degradedPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

type consoleNotificationController struct {
	dynamicClient        dynamic.Interface
	recorder             events.Recorder
	pacemakerInformer    cache.SharedIndexInformer
	clusterVersionLister configv1listers.ClusterVersionLister
	consoleUnavailable   bool
}

func NewConsoleNotificationController(
	pacemakerInformer cache.SharedIndexInformer,
	dynamicClient dynamic.Interface,
	clusterVersionLister configv1listers.ClusterVersionLister,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &consoleNotificationController{
		dynamicClient:        dynamicClient,
		recorder:             eventRecorder,
		pacemakerInformer:    pacemakerInformer,
		clusterVersionLister: clusterVersionLister,
	}

	return factory.New().
		ResyncEvery(HealthCheckResyncInterval).
		WithInformers(pacemakerInformer).
		WithSync(c.sync).
		ToController("ConsoleNotificationController", eventRecorder.WithComponentSuffix("console-notification"))
}

func (c *consoleNotificationController) docsBaseURL() string {
	version := "latest"
	cv, err := c.clusterVersionLister.Get("version")
	if err != nil {
		klog.V(4).Infof("Failed to get ClusterVersion for docs URL, using %q: %v", version, err)
		return fmt.Sprintf(docsURLFormat, version)
	}
	if len(cv.Status.History) > 0 {
		if parts := strings.SplitN(cv.Status.History[0].Version, ".", 3); len(parts) >= 2 {
			version = parts[0] + "." + parts[1]
		}
	}
	return fmt.Sprintf(docsURLFormat, version)
}

func (c *consoleNotificationController) sync(ctx context.Context, _ factory.SyncContext) error {
	if c.consoleUnavailable {
		return nil
	}

	item, exists, err := c.pacemakerInformer.GetStore().GetByKey(PacemakerClusterResourceName)
	if err != nil {
		return err
	}
	if !exists {
		return c.deleteAllNotifications(ctx)
	}

	cr, ok := item.(*pacmkrv1.PacemakerCluster)
	if !ok {
		return fmt.Errorf("unexpected object type in informer store: %T", item)
	}

	if cr.Status.LastUpdated.IsZero() {
		return c.deleteAllNotifications(ctx)
	}

	status := BuildHealthStatusFromCR(cr)
	degradedProblems, troubleshootingProblems := classifyProblems(status)

	if err := c.manageNotification(ctx, categoryDegraded, degradedProblems); err != nil {
		return err
	}
	return c.manageNotification(ctx, categoryTroubleshooting, troubleshootingProblems)
}

func (c *consoleNotificationController) manageNotification(ctx context.Context, cat notificationCategory, problems []string) error {
	if len(problems) == 0 {
		return c.deleteNotification(ctx, cat)
	}
	return c.ensureNotification(ctx, cat, problems)
}

func (c *consoleNotificationController) ensureNotification(ctx context.Context, cat notificationCategory, problems []string) error {
	text := strings.Join(problems, ". ") + ". Check pacemaker status for details."
	linkHref := c.docsBaseURL() + cat.docsFragment

	u, err := buildNotificationUnstructured(cat, linkHref, text)
	if err != nil {
		return err
	}

	_, _, err = resourceapply.ApplyUnstructuredResourceImproved(
		ctx, c.dynamicClient, c.recorder, u, nil, consoleNotificationGVR, nil, nil)
	return c.filterConsoleError(err)
}

func (c *consoleNotificationController) deleteNotification(ctx context.Context, cat notificationCategory) error {
	u := &unstructured.Unstructured{}
	u.SetName(cat.name)
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "console.openshift.io",
		Version: "v1",
		Kind:    "ConsoleNotification",
	})

	_, _, err := resourceapply.DeleteUnstructuredResource(
		ctx, c.dynamicClient, c.recorder, u, consoleNotificationGVR)
	return c.filterConsoleError(err)
}

func (c *consoleNotificationController) deleteAllNotifications(ctx context.Context) error {
	for _, cat := range allCategories {
		if err := c.deleteNotification(ctx, cat); err != nil {
			return err
		}
	}
	return nil
}

func (c *consoleNotificationController) filterConsoleError(err error) error {
	if err == nil {
		return nil
	}
	if meta.IsNoMatchError(err) {
		klog.Infof("Console API not available on this cluster, disabling notification management")
		c.consoleUnavailable = true
		return nil
	}
	return err
}

func buildNotificationUnstructured(cat notificationCategory, linkHref, text string) (*unstructured.Unstructured, error) {
	notification := &consolev1.ConsoleNotification{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "console.openshift.io/v1",
			Kind:       "ConsoleNotification",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: cat.name,
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
				Href: linkHref,
				Text: cat.linkText,
			},
		},
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(notification)
	if err != nil {
		return nil, fmt.Errorf("failed to convert ConsoleNotification to unstructured: %w", err)
	}
	return &unstructured.Unstructured{Object: obj}, nil
}
