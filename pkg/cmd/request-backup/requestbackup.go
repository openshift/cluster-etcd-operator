package requestbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	goflag "flag"
	"fmt"
	"iter"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorversionedclientv1alpha1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/pager"
	"k8s.io/klog/v2"
)

// EtcdBackup API restricts name length to 63 so it can be used in label values
const maxNameLength = 63

var errSkipExecution = errors.New("skip schedule execution")

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

type requestBackupOpts struct {
	etcdBackupName       string
	etcdBackupPolicyName string
	pvcName              string
	pvcPath              string
	hostPath             string
	kubeConfig           string
}

func NewRequestBackupCommand(ctx context.Context) *cobra.Command {
	opts := requestBackupOpts{}
	cmd := &cobra.Command{
		Use:   "request-backup",
		Short: "Requests a one time etcd backup by creating an operator.openshift.io/v1alpha1 EtcdBackup CustomResource",
		Run: func(cmd *cobra.Command, args []string) {
			defer klog.Flush()

			if err := opts.Validate(); err != nil {
				klog.Fatal(err)
			}
			if err := opts.Run(ctx); err != nil {
				klog.Fatal(err)
			}
		},
	}

	opts.AddFlags(cmd)
	return cmd
}

func (r *requestBackupOpts) AddFlags(cmd *cobra.Command) {
	flagSet := cmd.Flags()

	flagSet.StringVar(&r.etcdBackupName, "name", "", "name specifies the name of the EtcdBackup CR. Must be 63 characters or less.")
	flagSet.StringVar(&r.etcdBackupPolicyName, "policy", "", "policy specifies the name of the EtcdBackupPolicy CR that the EtcdBackup is created for. It is incompatible with the storage arguments pvc-name, pvc-path, and host-path.")
	flagSet.StringVar(&r.pvcName, "pvc-name", "", "pvc-name specifies the name of the PersistentVolumeClaim (PVC) which binds a PersistentVolume where the etcd backup file would be saved")
	flagSet.StringVar(&r.pvcPath, "pvc-path", "", "pvc-path specifies the directory on the PVC where the etcd backup file would be saved")
	flagSet.StringVar(&r.hostPath, "host-path", "", "host-path specifies the directory on the host where the etcd backup file would be saved")

	flagSet.StringVar(&r.kubeConfig, "kubeconfig", "", "Optional kubeconfig specifies the kubeConfig for when the cmd is running outside of a cluster")

	cobra.MarkFlagRequired(flagSet, "name")

	// adding klog flags to tune verbosity better
	gfs := goflag.NewFlagSet("", goflag.ExitOnError)
	klog.InitFlags(gfs)
	cmd.Flags().AddGoFlagSet(gfs)
}

func (r *requestBackupOpts) Validate() error {
	if len(r.etcdBackupName) > maxNameLength {
		return fmt.Errorf("--name length must be 63 characters or less")
	}
	if r.etcdBackupPolicyName != "" && (r.pvcName != "" || r.hostPath != "") {
		return fmt.Errorf("--pvc-name and --host-path are incompatible with --policy")
	}
	if r.pvcName == "" && r.hostPath == "" && r.etcdBackupPolicyName == "" {
		return fmt.Errorf("--pvc-name, --host-path or --policy must be set")
	}
	if (r.pvcName != "" || r.pvcPath != "") && r.hostPath != "" {
		return fmt.Errorf("--pvc-name and --pvc-path are incompatible with --host-path")
	}

	return nil
}

func (r *requestBackupOpts) Run(ctx context.Context) error {
	if r.pvcName == "" && r.hostPath == "" && r.etcdBackupPolicyName == "" {
		errMsg := "pvcName or hostPath must be specified to execute a backup request"
		klog.Error(errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	// handle teardown
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	shutdownHandler := make(chan os.Signal, 2)
	signal.Notify(shutdownHandler, shutdownSignals...)
	go func() {
		select {
		case <-shutdownHandler:
			klog.Infof("Received SIGTERM or SIGINT signal, shutting down.")
			close(shutdownHandler)
			cancel()
		case <-ctx.Done():
			klog.Infof("Context has been cancelled, shutting down.")
			close(shutdownHandler)
			cancel()
		}
	}()

	// Setup the EtcdBackup client
	kubeConfig, err := clientcmd.BuildConfigFromFlags("", r.kubeConfig)
	if err != nil {
		klog.Errorf("error loading kubeconfig: %v", err)
		return fmt.Errorf("error loading kubeconfig: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}
	operatorClient, err := operatorversionedclientv1alpha1.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	return r.requestBackups(ctx, kubeClient, operatorClient)
}

func (r *requestBackupOpts) requestBackups(ctx context.Context, kubeClient kubernetes.Interface, operatorClient operatorversionedclientv1alpha1.OperatorV1alpha1Interface) error {
	etcdBackupClient := operatorClient.EtcdBackups()

	if r.etcdBackupPolicyName != "" {
		nodeClient := kubeClient.CoreV1().Nodes()
		etcdBackupPolicyClient := operatorClient.EtcdBackupPolicies()
		backupPolicy, err := etcdBackupPolicyClient.Get(ctx, r.etcdBackupPolicyName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if backupPolicy.DeletionTimestamp != nil {
			return nil
		}
		return createBackupsForPolicy(ctx, r.etcdBackupName, backupPolicy, etcdBackupClient, nodeClient)
	}

	return createBackup(ctx, r.etcdBackupName, r.pvcName, r.pvcPath, r.hostPath, etcdBackupClient)
}

func createBackup(ctx context.Context, name, pvcName, pvcPath, hostPath string, backupClient operatorversionedclientv1alpha1.EtcdBackupInterface) error {
	// Create the EtcdBackup CR
	etcdBackup := &operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	if pvcName != "" {
		etcdBackup.Spec.Storage = operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC: operatorv1alpha1.EtcdBackupStoragePvc{
				Name: pvcName,
				Path: pvcPath,
			},
		}
	} else {
		etcdBackup.Spec.Storage = operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
			Local: operatorv1alpha1.EtcdBackupStorageLocal{
				HostPath: hostPath,
			},
		}
	}

	klog.Infof("creating CRD: %v", etcdBackup)
	if _, err := backupClient.Create(ctx, etcdBackup, metav1.CreateOptions{}); err != nil {
		klog.Errorf("failed to create EtcdBackup CR: %v", err)
		return err
	}
	return nil
}

// createBackupsForPolicy creates EtcdBackup resources for the current schedule execution of an EtcdBackupPolicy
func createBackupsForPolicy(ctx context.Context, name string, backupPolicy *operatorv1alpha1.EtcdBackupPolicy, backupClient operatorversionedclientv1alpha1.EtcdBackupInterface, nodeClient corev1client.NodeInterface) error {
	// Get control plane nodes
	nodeSelector := labels.Set{backuphelpers.ControlPlaneNodeLabelSelector: ""}
	if len(backupPolicy.Spec.NodeSelector) > 0 {
		maps.Copy(nodeSelector, backupPolicy.Spec.NodeSelector)
	}
	nodeList, err := nodeClient.List(ctx, metav1.ListOptions{
		LabelSelector: nodeSelector.String(),
	})

	if err != nil {
		return fmt.Errorf("failed to select control plane nodes for backup: %w", err)
	}
	if len(nodeList.Items) == 0 {
		klog.Warningf("No control plane nodes found for backup %s, skipping this execution", backupPolicy.Name)
		return nil
	}

	isAllNodes := backupPolicy.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypeLocal && backupPolicy.Spec.Storage.Local.Distribution == operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeAllNodes
	existingLocalBackupHostnames := map[string]struct{}{}
	var latestLocalBackup localBackup

	backupPager := pager.New(func(ctx context.Context, opts v1.ListOptions) (runtime.Object, error) {
		return backupClient.List(ctx, opts)
	})
	err = backupPager.EachListItem(ctx, v1.ListOptions{
		LabelSelector: backuphelpers.LabelEtcdBackupPolicy + "=" + backupPolicy.Name,
	}, func(obj runtime.Object) error {
		backup, ok := obj.(*operatorv1alpha1.EtcdBackup)
		if !ok {
			return fmt.Errorf("expected operator.openshift.io/v1alpha1 EtcdBackup, got %s", obj.GetObjectKind().GroupVersionKind().String())
		}

		if isAllNodes {
			// In Local AllNodes mode, ensure a backup exists for each selected node even if previously running backups exist for some nodes.
			// If backups are partially created and then a transient error occurs, the LabelEtcdBackupPolicyRun check ensures we retry idempotently.
			// If backups on a single node are indefinitely stuck, this ensures backups are still taken on other nodes.
			if backup.Labels[backuphelpers.LabelEtcdBackupPolicyRun] == name || !backuphelpers.IsBackupFinished(backup) {
				// Only check for local backups in case the policy switched from PVC -> Local AllNodes.
				// Even if the PVC backup was assigned to a specific node, it should not count against the backups to be created for the Local policy.
				if hostname := backup.Spec.NodeSelector[corev1.LabelHostname]; backup.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypeLocal && hostname != "" {
					existingLocalBackupHostnames[hostname] = struct{}{}
				}
			}
		} else if !backuphelpers.IsBackupFinished(backup) {
			klog.Infof("skipping schedule execution for EtcdBackupPolicy %s: active backups found", backupPolicy.Name)
			return errSkipExecution
		} else if backup.Labels[backuphelpers.LabelEtcdBackupPolicyRun] == name {
			klog.Infof("skipping schedule execution for EtcdBackupPolicy %s: existing backup found for this run", backupPolicy.Name)
			return errSkipExecution
		}

		// Find latest local backup for Local SingleNode round robin
		if backup.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypeLocal && backup.Spec.NodeSelector[corev1.LabelHostname] != "" {
			if latestLocalBackup.name == "" ||
				backup.CreationTimestamp.After(latestLocalBackup.creationTimestamp) ||
				(backup.CreationTimestamp.Time.Equal(latestLocalBackup.creationTimestamp) && backup.Name > latestLocalBackup.name) {
				latestLocalBackup = localBackup{
					name:              backup.Name,
					creationTimestamp: backup.CreationTimestamp.Time,
					nodeName:          backup.Status.NodeName,
					hostname:          backup.Spec.NodeSelector[corev1.LabelHostname],
				}
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errSkipExecution) {
			return nil
		}
		return err
	}

	// Build the set of EtcdBackups to create. Local backups are taken once per control plane node and pinned to that
	// node (the hostPath is node-specific). PVC backups are node-independent: a single backup is created and
	// the scheduler decides placement based on the volume's topology.
	var toCreate []*operatorv1alpha1.EtcdBackup
	switch backupPolicy.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		toCreate = append(toCreate, &operatorv1alpha1.EtcdBackup{
			ObjectMeta: v1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					backuphelpers.LabelEtcdBackupPolicy:    backupPolicy.Name,
					backuphelpers.LabelEtcdBackupPolicyRun: name,
				},
			},
			Spec: operatorv1alpha1.EtcdBackupSpec{
				Assets:       backupPolicy.Spec.Assets,
				NodeSelector: backupPolicy.Spec.NodeSelector,
				Storage:      backuphelpers.EtcdBackupStorageFromPolicy(backupPolicy),
			},
		})
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		for node := range iterLocalNodes(backupPolicy, latestLocalBackup, nodeList) {
			hostName := node.Labels[corev1.LabelHostname]
			if _, ok := existingLocalBackupHostnames[hostName]; ok {
				continue
			}

			backupName := name
			suffix := "-" + shortHash(name, node.Name)
			remaining := maxNameLength - len(suffix)
			if len(backupName) > remaining {
				backupName = strings.TrimSuffix(backupName[:remaining], "-")
			}
			backupName += suffix

			toCreate = append(toCreate, &operatorv1alpha1.EtcdBackup{
				ObjectMeta: v1.ObjectMeta{
					Name: backupName,
					Labels: map[string]string{
						backuphelpers.LabelEtcdBackupPolicy:    backupPolicy.Name,
						backuphelpers.LabelEtcdBackupPolicyRun: name,
					},
				},
				Spec: operatorv1alpha1.EtcdBackupSpec{
					Assets:       backupPolicy.Spec.Assets,
					NodeSelector: map[string]string{corev1.LabelHostname: node.Labels[corev1.LabelHostname]},
					Storage:      backuphelpers.EtcdBackupStorageFromPolicy(backupPolicy),
				},
			})
		}
	default:
		return fmt.Errorf("unknown EtcdBackupStorageType: %q", backupPolicy.Spec.Storage.Type)
	}

	var errs []error
	for _, backup := range toCreate {
		backupName := backup.Name
		if _, err := backupClient.Create(ctx, backup, v1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Check existing during name conflict. If it belongs to this policy run, then ignore the error.
				existingBackup, getErr := backupClient.Get(ctx, backup.Name, v1.GetOptions{})
				if getErr != nil {
					errs = append(errs, fmt.Errorf("failed to fetch backup with conflicting name %q: %w", backup.Name, getErr))
				} else if existingBackup.Labels[backuphelpers.LabelEtcdBackupPolicy] != backupPolicy.Name || existingBackup.Labels[backuphelpers.LabelEtcdBackupPolicyRun] != name {
					errs = append(errs, fmt.Errorf("etcdbackup name conflict on %q: existing backup does not belong to this policy run: %w", backup.Name, err))
				}
			} else {
				klog.Warningf("Failed to create EtcdBackup %q: %v", backupName, err)
				errs = append(errs, err)
			}
		} else {
			klog.Infof("created EtcdBackup %q", backupName)
		}
	}
	return utilerrors.NewAggregate(errs)
}

func iterLocalNodes(backupPolicy *operatorv1alpha1.EtcdBackupPolicy, latestLocalBackup localBackup, nodeList *corev1.NodeList) iter.Seq[corev1.Node] {
	return func(yield func(corev1.Node) bool) {
		switch backupPolicy.Spec.Storage.Local.Distribution {
		case "", operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeSingleNode:
			if node, ok := nextRoundRobinNode(latestLocalBackup, nodeList); ok {
				yield(node)
			}
		case operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeAllNodes:
			for _, node := range nodeList.Items {
				if hostname := node.Labels[corev1.LabelHostname]; hostname != "" {
					if !yield(node) {
						return
					}
				}
			}
		}
	}
}

// nextRoundRobinNode selects the node for a Local SingleNode backup. Nodes are ordered lexicographically by name, and
// the node following the one targeted by the policy's most recently created backup is selected, wrapping around to
// the first node. Comparing by name (rather than by index) keeps the rotation stable when nodes are added or removed:
// if the previously targeted node no longer exists, the next node name after it is still selected.
//
// The most recent backup is used regardless of outcome so that a node which persistently fails backups does not
// prevent the remaining nodes from being rotated through.
func nextRoundRobinNode(latestLocalBackup localBackup, nodeList *corev1.NodeList) (corev1.Node, bool) {
	nodes := make([]corev1.Node, 0, len(nodeList.Items))
	for _, node := range nodeList.Items {
		if node.Labels[corev1.LabelHostname] != "" {
			nodes = append(nodes, node)
		}
	}
	if len(nodes) == 0 {
		return corev1.Node{}, false
	}
	slices.SortFunc(nodes, func(a, b corev1.Node) int {
		return strings.Compare(a.Name, b.Name)
	})

	latestNodeName := latestNodeName(latestLocalBackup, nodeList)
	if latestNodeName == "" {
		return nodes[0], true
	}
	for _, node := range nodes {
		if node.Name > latestNodeName {
			return node, true
		}
	}
	return nodes[0], true
}

// latestNodeName returns the name of the node targeted by the most recently created node-pinned backup in
// the list, or an empty string if there is none.
//
// Backups are pinned by hostname label, and the node name is only recorded in status once the backup job has been
// scheduled. If the status is not yet populated, the node name is resolved from the hostname via the given nodes,
// falling back to the hostname itself when the node no longer exists (node name and hostname typically match).
func latestNodeName(latestLocalBackup localBackup, nodeList *corev1.NodeList) string {
	if latestLocalBackup.nodeName != "" {
		return latestLocalBackup.nodeName
	}

	if latestLocalBackup.hostname != "" {
		// In case nodeName status wasn't set on backup, fallback to finding node by hostname
		for _, node := range nodeList.Items {
			if node.Labels[corev1.LabelHostname] == latestLocalBackup.hostname {
				return node.Name
			}
		}
	}

	return latestLocalBackup.hostname
}

func shortHash(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "")))
	shortHash := hex.EncodeToString(hash[:])[:10]
	return shortHash
}

type localBackup struct {
	name               string
	creationTimestamp  time.Time
	nodeName, hostname string
}
