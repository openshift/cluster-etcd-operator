package requestbackup

import (
	"context"
	goflag "flag"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"syscall"

	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorversionedclientv1alpha1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

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

	flagSet.StringVar(&r.etcdBackupName, "name", "", "name specifies the name of the EtcdBackup CR.")
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

// executeBackup creates EtcdBackup resources the current schedule execution
func createBackupsForPolicy(ctx context.Context, name string, backupPolicy *operatorv1alpha1.EtcdBackupPolicy, backupClient operatorversionedclientv1alpha1.EtcdBackupInterface, nodeClient corev1client.NodeInterface) error {
	// Check for active backups that belong to the policy
	backupList, err := backupClient.List(ctx, v1.ListOptions{
		LabelSelector: backuphelpers.LabelEtcdBackupPolicy + "=" + backupPolicy.Name,
	})
	if err != nil {
		return err
	}
	for _, backup := range backupList.Items {
		if !backuphelpers.IsBackupFinished(&backup) {
			klog.Infof("skipping schedule execution for EtcdBackupPolicy %s: active backups found", backupPolicy.Name)
			return nil
		}
	}

	// Get control plane nodes
	nodeSelector := labels.Set{backuphelpers.ControlPlaneNodeLabelSelector: ""}
	maps.Copy(nodeSelector, backupPolicy.Spec.NodeSelector)
	nodeList, err := nodeClient.List(ctx, metav1.ListOptions{
		LabelSelector: nodeSelector.String(),
	})

	if err != nil {
		return fmt.Errorf("BackupPolicyController failed to select control plane nodes for backup: %w", err)
	}
	if len(nodeList.Items) == 0 {
		klog.Warningf("No control plane nodes found for backup %s, skipping this execution", backupPolicy.Name)
		return nil
	}

	// Build the set of EtcdBackups to create. Local backups are taken once per control plane node and pinned to that
	// node (the hostPath is node-specific). PVC backups are node-independent: a single backup is created and
	// the scheduler decides placement based on the volume's topology.
	var toCreate []*operatorv1alpha1.EtcdBackup
	if backupPolicy.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypePVC {
		toCreate = append(toCreate, &operatorv1alpha1.EtcdBackup{
			ObjectMeta: v1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					backuphelpers.LabelEtcdBackupPolicy: backupPolicy.Name,
				},
			},
			Spec: operatorv1alpha1.EtcdBackupSpec{
				NodeSelector: backupPolicy.Spec.NodeSelector,
				Storage:      backupPolicy.Spec.Storage,
			},
		})
	} else {
		for _, node := range nodeList.Items {
			if hostname, ok := node.Labels[corev1.LabelHostname]; ok {
				toCreate = append(toCreate, &operatorv1alpha1.EtcdBackup{
					ObjectMeta: v1.ObjectMeta{
						Name: name + "-" + string(node.UID),
						Labels: map[string]string{
							backuphelpers.LabelEtcdBackupPolicy: backupPolicy.Name,
						},
					},
					Spec: operatorv1alpha1.EtcdBackupSpec{
						NodeSelector: map[string]string{corev1.LabelHostname: hostname},
						Storage:      backupPolicy.Spec.Storage,
					},
				})
			}
		}
	}

	for _, etcdBackup := range toCreate {
		backupName := etcdBackup.Name
		if _, err := backupClient.Create(ctx, etcdBackup, v1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				klog.Warningf("Failed to create EtcdBackup %q: %v", backupName, err)
			}
		} else {
			klog.Infof("BackupPolicyController created EtcdBackup %q", backupName)
		}
	}
	return nil
}
