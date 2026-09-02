package requestbackup

import (
	"context"
	goflag "flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorversionedclientv1alpha1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

type requestBackupOpts struct {
	etcdBackupName string
	pvcName        string
	pvcPath        string
	hostPath       string
	kubeConfig     string
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
	if r.pvcName == "" && r.hostPath == "" {
		return fmt.Errorf("--pvc-name or --host-path must be set")
	}
	if (r.pvcName != "" || r.pvcPath != "") && r.hostPath != "" {
		return fmt.Errorf("--pvc-name and --pvc-path are incompatible with --host-path")
	}

	return nil
}

func (r *requestBackupOpts) Run(ctx context.Context) error {
	if r.pvcName == "" && r.hostPath == "" {
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
	operatorClient, err := operatorversionedclientv1alpha1.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}
	etcdBackupClient := operatorClient.EtcdBackups()

	// Create the EtcdBackup CR
	// TODO(haseeb): This EtcdBackup manifest is small enough but should we template this manifest from bindata/etcd
	// like we usually do for other manifests?
	etcdBackup := &operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.etcdBackupName,
			Namespace: operatorclient.TargetNamespace,
		},
	}
	if r.pvcName != "" {
		etcdBackup.Spec.Storage = operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
				Name: r.pvcName,
				Path: r.pvcPath,
			},
		}
	} else {
		etcdBackup.Spec.Storage = operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
			Local: &operatorv1alpha1.EtcdBackupStorageLocal{
				HostPath: r.hostPath,
			},
		}
	}

	klog.Infof("creating CRD: %v", etcdBackup)
	_, err = etcdBackupClient.Create(ctx, etcdBackup, metav1.CreateOptions{})
	if err != nil {
		klog.Errorf("failed to create EtcdBackup CR: %v", err)
		return err
	}

	return nil
}
