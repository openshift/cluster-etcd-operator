package pacemakerstatuscollector

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pacemaker/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

const (
	// pcsStatusXMLCommand is the command to get pacemaker status in XML format
	pcsStatusXMLCommand = "sudo -n pcs status xml"

	// execTimeout is the timeout for executing the pcs command
	execTimeout = 10 * time.Second

	// maxXMLSize prevents XML bombs (10MB)
	maxXMLSize = 10 * 1024 * 1024

	// pacemakerStatusName is the name of the singleton PacemakerStatus CR
	pacemakerStatusName = "cluster"

	// Time windows for detecting recent events
	failedActionTimeWindow = 5 * time.Minute
	fencingEventTimeWindow = 24 * time.Hour

	// Pacemaker state strings
	booleanValueTrue       = "true"
	operationRCSuccess     = "0"
	operationRCTextSuccess = "ok"
	nodeStatusStarted      = "Started"

	// Time formats for parsing Pacemaker timestamps
	pacemakerTimeFormat      = "Mon Jan 2 15:04:05 2006"
	pacemakerFenceTimeFormat = "2006-01-02 15:04:05.000000Z"
)

// XML structures for parsing pacemaker status
// These are copied from healthcheck.go to avoid circular dependencies
type PacemakerResult struct {
	XMLName      xml.Name     `xml:"pacemaker-result"`
	Summary      Summary      `xml:"summary"`
	Nodes        Nodes        `xml:"nodes"`
	Resources    Resources    `xml:"resources"`
	NodeHistory  NodeHistory  `xml:"node_history"`
	FenceHistory FenceHistory `xml:"fence_history"`
}

type Summary struct {
	Stack               Stack               `xml:"stack"`
	CurrentDC           CurrentDC           `xml:"current_dc"`
	NodesConfigured     NodesConfigured     `xml:"nodes_configured"`
	ResourcesConfigured ResourcesConfigured `xml:"resources_configured"`
}

type Stack struct {
	PacemakerdState string `xml:"pacemakerd-state,attr"`
}

type CurrentDC struct {
	WithQuorum string `xml:"with_quorum,attr"`
}

type NodesConfigured struct {
	Number string `xml:"number,attr"`
}

type ResourcesConfigured struct {
	Number string `xml:"number,attr"`
}

type Nodes struct {
	Node []Node `xml:"node"`
}

type Node struct {
	Online string `xml:"online,attr"`
}

type Resources struct {
	Clone    []Clone    `xml:"clone"`
	Resource []Resource `xml:"resource"`
}

type Clone struct {
	Resource []Resource `xml:"resource"`
}

type Resource struct {
	ResourceAgent string  `xml:"resource_agent,attr"`
	Role          string  `xml:"role,attr"`
	Active        string  `xml:"active,attr"`
	Node          NodeRef `xml:"node"`
}

type NodeRef struct {
	Name string `xml:"name,attr"`
}

type NodeHistory struct {
	Node []NodeHistoryNode `xml:"node"`
}

type NodeHistoryNode struct {
	ResourceHistory []ResourceHistory `xml:"resource_history"`
}

type ResourceHistory struct {
	OperationHistory []OperationHistory `xml:"operation_history"`
}

type OperationHistory struct {
	RC           string `xml:"rc,attr"`
	RCText       string `xml:"rc_text,attr"`
	LastRCChange string `xml:"last-rc-change,attr"`
}

type FenceHistory struct {
	FenceEvent []FenceEvent `xml:"fence_event"`
}

type FenceEvent struct {
	Completed string `xml:"completed,attr"`
}

// NewPacemakerStatusCollectorCommand creates a new command for collecting pacemaker status
func NewPacemakerStatusCollectorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pacemaker-status-collector",
		Short: "Collects pacemaker status and updates PacemakerStatus CR",
		Run: func(cmd *cobra.Command, args []string) {
			if err := runCollector(); err != nil {
				klog.Errorf("Failed to collect pacemaker status: %v", err)
				os.Exit(1)
			}
		},
	}
	return cmd
}

func runCollector() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	klog.Info("Starting pacemaker status collection...")

	// Collect pacemaker status
	rawXML, summary, collectionError := collectPacemakerStatus(ctx)

	// Create Kubernetes client
	client, err := createKubernetesClient()
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// Update PacemakerStatus CR
	if err := updatePacemakerStatusCR(ctx, client, rawXML, summary, collectionError); err != nil {
		return fmt.Errorf("failed to update PacemakerStatus CR: %w", err)
	}

	klog.Info("Successfully updated PacemakerStatus CR")
	return nil
}

func collectPacemakerStatus(ctx context.Context) (rawXML string, summary v1alpha1.PacemakerSummary, collectionError string) {
	// Execute the pcs status xml command with a timeout
	ctxExec, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	stdout, stderr, err := exec.Execute(ctxExec, pcsStatusXMLCommand)
	if err != nil {
		collectionError = fmt.Sprintf("Failed to execute pcs status xml command: %v", err)
		klog.Warning(collectionError)
		return "", summary, collectionError
	}

	if stderr != "" {
		klog.Warningf("pcs status xml command produced stderr: %s", stderr)
	}

	// Validate XML size to prevent XML bombs
	if len(stdout) > maxXMLSize {
		collectionError = fmt.Sprintf("XML output too large: %d bytes (max: %d bytes)", len(stdout), maxXMLSize)
		klog.Warning(collectionError)
		return "", summary, collectionError
	}

	rawXML = stdout

	// Parse the XML to create a summary
	var result PacemakerResult
	if err := xml.Unmarshal([]byte(rawXML), &result); err != nil {
		collectionError = fmt.Sprintf("Failed to parse XML: %v", err)
		klog.Warning(collectionError)
		// Still return the raw XML even if parsing fails
		return rawXML, summary, collectionError
	}

	// Build summary
	summary = buildSummary(&result)

	return rawXML, summary, ""
}

func buildSummary(result *PacemakerResult) v1alpha1.PacemakerSummary {
	summary := v1alpha1.PacemakerSummary{
		PacemakerdState: result.Summary.Stack.PacemakerdState,
		HasQuorum:       result.Summary.CurrentDC.WithQuorum == booleanValueTrue,
	}

	// Count online nodes
	onlineCount := 0
	for _, node := range result.Nodes.Node {
		if node.Online == booleanValueTrue {
			onlineCount++
		}
	}
	summary.NodesOnline = onlineCount
	summary.NodesTotal = len(result.Nodes.Node)

	// Count started resources
	resourcesStarted := 0
	resourcesTotal := 0

	// Count clone resources
	for _, clone := range result.Resources.Clone {
		for _, resource := range clone.Resource {
			resourcesTotal++
			if resource.Role == nodeStatusStarted && resource.Active == booleanValueTrue {
				resourcesStarted++
			}
		}
	}

	// Count standalone resources
	for _, resource := range result.Resources.Resource {
		resourcesTotal++
		if resource.Role == nodeStatusStarted && resource.Active == booleanValueTrue {
			resourcesStarted++
		}
	}

	summary.ResourcesStarted = resourcesStarted
	summary.ResourcesTotal = resourcesTotal

	// Check for recent failures
	summary.RecentFailures = hasRecentFailures(result)
	summary.RecentFencing = hasRecentFencing(result)

	return summary
}

func hasRecentFailures(result *PacemakerResult) bool {
	cutoffTime := time.Now().Add(-failedActionTimeWindow)

	for _, node := range result.NodeHistory.Node {
		for _, resourceHistory := range node.ResourceHistory {
			for _, operation := range resourceHistory.OperationHistory {
				if operation.RC != operationRCSuccess || operation.RCText != operationRCTextSuccess {
					t, err := time.Parse(pacemakerTimeFormat, operation.LastRCChange)
					if err != nil {
						continue
					}
					if t.After(cutoffTime) {
						return true
					}
				}
			}
		}
	}
	return false
}

func hasRecentFencing(result *PacemakerResult) bool {
	cutoffTime := time.Now().Add(-fencingEventTimeWindow)

	for _, fenceEvent := range result.FenceHistory.FenceEvent {
		t, err := time.Parse(pacemakerFenceTimeFormat, fenceEvent.Completed)
		if err != nil {
			continue
		}
		if t.After(cutoffTime) {
			return true
		}
	}
	return false
}

func createKubernetesClient() (*kubernetes.Clientset, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.Getenv("HOME") + "/.kube/config"
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
	}

	return kubernetes.NewForConfig(config)
}

func updatePacemakerStatusCR(ctx context.Context, client *kubernetes.Clientset, rawXML string, summary v1alpha1.PacemakerSummary, collectionError string) error {
	// Create a REST client for the custom resource
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.Getenv("HOME") + "/.kube/config"
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return err
		}
	}

	// Set up the scheme and codec
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	config.GroupVersion = &v1alpha1.SchemeGroupVersion
	config.APIPath = "/apis"
	config.NegotiatedSerializer = serializer.NewCodecFactory(scheme)

	restClient, err := rest.RESTClientFor(config)
	if err != nil {
		return fmt.Errorf("failed to create REST client: %w", err)
	}

	// Try to get existing PacemakerStatus
	var existing v1alpha1.PacemakerStatus
	err = restClient.Get().
		Resource("pacemakerstatuses").
		Name(pacemakerStatusName).
		Do(ctx).
		Into(&existing)

	now := metav1.Now()

	if err != nil {
		// Create new PacemakerStatus if it doesn't exist
		if strings.Contains(err.Error(), "not found") {
			newStatus := &v1alpha1.PacemakerStatus{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerStatus",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: pacemakerStatusName,
				},
				Status: v1alpha1.PacemakerStatusStatus{
					LastUpdated:     now,
					RawXML:          rawXML,
					CollectionError: collectionError,
					Summary:         summary,
				},
			}

			result := restClient.Post().
				Resource("pacemakerstatuses").
				Body(newStatus).
				Do(ctx)

			if result.Error() != nil {
				return fmt.Errorf("failed to create PacemakerStatus: %w", result.Error())
			}

			klog.Info("Created new PacemakerStatus CR")
			return nil
		}
		return fmt.Errorf("failed to get PacemakerStatus: %w", err)
	}

	// Update existing PacemakerStatus
	existing.Status.LastUpdated = now
	existing.Status.RawXML = rawXML
	existing.Status.CollectionError = collectionError
	existing.Status.Summary = summary
	existing.Status.ObservedGeneration = existing.Generation

	result := restClient.Put().
		Resource("pacemakerstatuses").
		Name(pacemakerStatusName).
		SubResource("status").
		Body(&existing).
		Do(ctx)

	if result.Error() != nil {
		return fmt.Errorf("failed to update PacemakerStatus: %w", result.Error())
	}

	klog.Info("Updated existing PacemakerStatus CR")
	return nil
}
