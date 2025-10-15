package jobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	// Default resync interval for the controller
	defaultResyncInterval = 1 * time.Minute

	// namespace for the cronjobs
	defaultNamespace = "openshift-etcd"
)

// CronJobController manages a Kubernetes CronJob resource
type CronJobController struct {
	operatorClient v1helpers.OperatorClient
	kubeClient     kubernetes.Interface
	eventRecorder  events.Recorder

	// Configuration for the CronJob
	manifest  []byte
	name      string
	namespace string
	schedule  string
	command   []string
	image     string
}

// CronJobConfig contains configuration for a CronJob
type CronJobConfig struct {
	Name      string
	Namespace string
	Schedule  string   // Cron schedule expression (e.g., "*/30 * * * * *" for every 30 seconds)
	Command   []string // Command to run in the container
	Image     string   // Container image to use
}

// NewCronJobController creates a new controller for managing a CronJob
func NewCronJobController(
	manifest []byte,
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	config CronJobConfig,
) factory.Controller {
	// Set defaults
	if config.Namespace == "" {
		config.Namespace = defaultNamespace
	}

	c := &CronJobController{
		operatorClient: operatorClient,
		kubeClient:     kubeClient,
		eventRecorder:  eventRecorder,
		manifest:       manifest,
		name:           config.Name,
		namespace:      config.Namespace,
		schedule:       config.Schedule,
		command:        config.Command,
		image:          config.Image,
	}

	controllerName := fmt.Sprintf("%sCronJobController", config.Name)
	syncCtx := factory.NewSyncContext(controllerName, eventRecorder.WithComponentSuffix(fmt.Sprintf("%s-cronjob", config.Name)))

	return factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(defaultResyncInterval).
		WithSync(c.sync).
		WithInformers(
			operatorClient.Informer(),
		).ToController(controllerName, syncCtx.Recorder())
}

func (c *CronJobController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("%s sync started", c.name)
	defer klog.V(4).Infof("%s sync completed", c.name)

	// Check operator state
	opSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}

	if opSpec.ManagementState != operatorv1.Managed {
		klog.V(4).Infof("Operator is not in Managed state, skipping CronJob creation")
		return nil
	}

	// Ensure the CronJob exists with the correct configuration
	if err := c.ensureCronJob(ctx); err != nil {
		return fmt.Errorf("failed to ensure CronJob: %w", err)
	}

	return nil
}

func (c *CronJobController) ensureCronJob(ctx context.Context) error {
	// Get existing CronJob if it exists
	existing, err := c.kubeClient.BatchV1().CronJobs(c.namespace).Get(ctx, c.name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get CronJob: %w", err)
		}
		// CronJob doesn't exist, create it
		return c.createCronJob(ctx)
	}

	// CronJob exists, check if it needs updating
	if c.needsUpdate(existing) {
		klog.V(2).Infof("CronJob %s needs updating", c.name)
		return c.updateCronJob(ctx, existing)
	}

	klog.V(4).Infof("CronJob %s is up to date", c.name)
	return nil
}

func (c *CronJobController) createCronJob(ctx context.Context) error {
	cronJob := c.buildCronJob()

	_, err := c.kubeClient.BatchV1().CronJobs(c.namespace).Create(ctx, cronJob, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			klog.V(4).Infof("CronJob %s already exists", c.name)
			return nil
		}
		return fmt.Errorf("failed to create CronJob: %w", err)
	}

	klog.V(2).Infof("Created CronJob: %s/%s with schedule %s", c.namespace, c.name, c.schedule)
	c.eventRecorder.Eventf("CronJobCreated", "Created CronJob: %s/%s", c.namespace, c.name)

	return nil
}

func (c *CronJobController) updateCronJob(ctx context.Context, existing *batchv1.CronJob) error {
	cronJob := c.buildCronJob()
	cronJob.ResourceVersion = existing.ResourceVersion

	_, err := c.kubeClient.BatchV1().CronJobs(c.namespace).Update(ctx, cronJob, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update CronJob: %w", err)
	}

	klog.V(2).Infof("Updated CronJob: %s/%s", c.namespace, c.name)
	c.eventRecorder.Eventf("CronJobUpdated", "Updated CronJob: %s/%s", c.namespace, c.name)

	return nil
}

func (c *CronJobController) buildCronJob() *batchv1.CronJob {
	// Inject values into the template
	injectedManifest := c.injectValues(c.manifest)

	// Log the manifest before parsing to help debug YAML errors
	klog.V(4).Infof("CronJob manifest before parsing:\n%s", string(injectedManifest))

	// Parse the manifest with injected values
	cronJob := ReadCronJobV1OrDie(injectedManifest)

	return cronJob
}

// injectValues replaces <injected> placeholders in the manifest with actual values
func (c *CronJobController) injectValues(manifest []byte) []byte {
	result := string(manifest)

	// Build command string for YAML array format
	var commandBuilder strings.Builder
	commandBuilder.WriteString("[")
	for i, cmd := range c.command {
		if i > 0 {
			commandBuilder.WriteString(", ")
		}
		commandBuilder.WriteString(fmt.Sprintf(`"%s"`, cmd))
	}
	commandBuilder.WriteString("]")
	commandStr := commandBuilder.String()

	// Track which line we're on to do context-aware replacement
	lines := strings.Split(result, "\n")
	for i, line := range lines {
		// Replace name (appears in metadata.name)
		if strings.Contains(line, "name: <injected>") {
			lines[i] = strings.Replace(line, "<injected>", c.name, 1)
		}
		// Replace app label
		if strings.Contains(line, "app.kubernetes.io/name: <injected>") {
			lines[i] = strings.Replace(line, "<injected>", c.name, 1)
		}
		// Replace schedule (must be quoted to handle special characters like *)
		if strings.Contains(line, "schedule: <injected>") {
			lines[i] = strings.Replace(line, "<injected>", fmt.Sprintf(`"%s"`, c.schedule), 1)
		}
		// Replace image
		if strings.Contains(line, "image: <injected>") {
			lines[i] = strings.Replace(line, "<injected>", c.image, 1)
		}
		// Replace command (special handling for array)
		if strings.Contains(line, "command: <injected>") {
			lines[i] = strings.Replace(line, "<injected>", commandStr, 1)
		}
	}

	return []byte(strings.Join(lines, "\n"))
}

func (c *CronJobController) needsUpdate(existing *batchv1.CronJob) bool {
	// Check if schedule has changed
	if existing.Spec.Schedule != c.schedule {
		return true
	}

	// Check if command has changed
	if len(existing.Spec.JobTemplate.Spec.Template.Spec.Containers) > 0 {
		container := existing.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
		if len(container.Command) != len(c.command) {
			return true
		}
		for i, cmd := range c.command {
			if i >= len(container.Command) || container.Command[i] != cmd {
				return true
			}
		}

		// Check if image has changed
		if container.Image != c.image {
			return true
		}
	}

	return false
}
