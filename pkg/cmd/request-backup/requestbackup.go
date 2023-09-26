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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	PodNameEnvVar = "MY_POD_NAME"
	PodUIDEnvVar  = "MY_POD_UID"
	JobNameEnvVar = "MY_JOB_NAME"
	JobUIDEnvVar  = "MY_JOB_UID"
)

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

type requestBackupOpts struct {
	etcdBackupName string
	pvcName        string
	kubeConfig     string
	ownerPodName   string
	ownerPodUID    string
	ownerJobName   string
	ownerJobUID    string
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

	flagSet.StringVar(&r.pvcName, "pvc-name", "", "pvc-name specifies the name of the PersistentVolumeClaim (PVC) which binds a PersistentVolume where the etcd backup file would be saved")
	cobra.MarkFlagRequired(flagSet, "pvcName")

	flagSet.StringVar(&r.kubeConfig, "kubeconfig", "", "Optional kubeconfig specifies the kubeConfig for when the cmd is running outside of a cluster")

	// adding klog flags to tune verbosity better
	gfs := goflag.NewFlagSet("", goflag.ExitOnError)
	klog.InitFlags(gfs)
	cmd.Flags().AddGoFlagSet(gfs)
}

func (r *requestBackupOpts) Validate() error {
	if r.pvcName == "" {
		return fmt.Errorf("--pvc-name must be set")
	}

	return nil
}

func (r *requestBackupOpts) ReadEnvVars() error {
	r.ownerPodName = os.Getenv(PodNameEnvVar)
	if r.ownerPodName == "" {
		return fmt.Errorf("pod name must be set via %v env var", PodNameEnvVar)
	}
	// The backup name is set to be the same as the pod name so that each scheduled run of the cronjob
	// executing this cmd results in a unique EtcdBackup name.
	r.etcdBackupName = r.ownerPodName

	r.ownerPodUID = os.Getenv(PodUIDEnvVar)
	if len(r.ownerPodUID) == 0 {
		return fmt.Errorf("pod UID must be set via %v env var", PodUIDEnvVar)
	}

	r.ownerJobName = os.Getenv(JobNameEnvVar)
	if r.ownerJobName == "" {
		return fmt.Errorf("job name must be set via %v env var", JobNameEnvVar)
	}

	r.ownerJobUID = os.Getenv(JobUIDEnvVar)
	if len(r.ownerJobUID) == 0 {
		return fmt.Errorf("job UID must be set via %v env var", JobUIDEnvVar)
	}

	return nil
}

func (r *requestBackupOpts) Run(ctx context.Context) error {
	if r.pvcName == "" {
		errMsg := "pvcName must be specified to execute a backup request"
		klog.Errorf(errMsg)
		return fmt.Errorf(errMsg)
	}

	// ReadEnvVars reads the env vars necessary to populate the ownerReference
	// and name for the EtcdBackup CR.
	if err := r.ReadEnvVars(); err != nil {
		errMsg := fmt.Sprintf("failed to read pod envvars %v", err)
		klog.Errorf(errMsg)
		return fmt.Errorf(errMsg)
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
			// Set the OwnerReference as the Pod executing this cmd so that when the Job (and Pod) for the scheduled run
			// is cleaned up, the EtcdBackup CR is also garbage collected.
			// This still leaves us with a history of EtcdBackup CRs as determined by the CronJob's successfulJobsHistoryLimit
			// and failedJobsHistoryLimit.
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       r.ownerPodName,
					UID:        types.UID(r.ownerPodUID),
				},
				{
					APIVersion: "batch/v1",
					Kind:       "Job",
					Name:       r.ownerJobName,
					UID:        types.UID(r.ownerJobUID),
				},
			},
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			PVCName: r.pvcName,
		},
	}

	_, err = etcdBackupClient.Create(ctx, etcdBackup, metav1.CreateOptions{})
	if err != nil {
		klog.Errorf("failed to create EtcdBackup CR: %v", err)
		return err
	}

	return nil
}
