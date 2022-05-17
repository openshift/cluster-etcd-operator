package monitoruploader

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	// we are syncing from disk, so there's no rush
	defaultSyncInterval = 60 * time.Second
	defaultLogOutputs   = "stderr"
	// number of bytes from the log that should be upload to the configmap every interval
	lastBytesToUpload = int64(50 * 1024)
)

type MonitorUploaderOptions struct {
	Interval              time.Duration
	LogInputs             string
	NumberOfBytesToUpload int64
	KubeConfig            string
	PodName               string

	ConfigMapNamespace string
	ConfigMapName      string
	ErrorOut           io.Writer
}

type MonitorUploader struct {
	Interval              time.Duration
	LogInput              string
	NumberOfBytesToUpload int64
	ErrorOut              io.Writer

	ConfigMapNamespace string
	ConfigMapName      string
	ConfigMapClient    coreclientv1.ConfigMapsGetter
	ConfigMapKey       string
	FieldManager       string
}

func NewMonitorUploaderOptions(errorOut io.Writer) *MonitorUploaderOptions {
	return &MonitorUploaderOptions{
		Interval:              defaultSyncInterval,
		LogInputs:             defaultLogOutputs,
		NumberOfBytesToUpload: lastBytesToUpload,
		KubeConfig:            "",
		ConfigMapNamespace:    "openshift-etcd-operator",
		ConfigMapName:         "etcd-health",
		PodName:               os.Getenv("POD_NAME"),
		ErrorOut:              errorOut,
	}
}

func NewMonitorCommand(errOut io.Writer) *cobra.Command {
	o := NewMonitorUploaderOptions(errOut)

	cmd := &cobra.Command{
		Use:   "monitor-uploader",
		Short: "Performs periodic upload of the written monitor file.",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func(ctx context.Context) error) {
				if err := fn(context.Background()); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
						fmt.Fprint(errOut, err.Error())
					}
				}
			}
			must(o.Validate)
			uploader, err := o.ToUploader(context.TODO())
			if err != nil {
				klog.Fatal(err)
			}
			must(uploader.Run)
		},
	}

	o.BindFlags(cmd.Flags())
	return cmd
}

func (o *MonitorUploaderOptions) BindFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.KubeConfig, "kubeconfig", o.KubeConfig, "kubeconfig to reach kube-apiserver")
	fs.StringVar(&o.ConfigMapNamespace, "configmap-namespace", o.ConfigMapNamespace, "namespace to write to")
	fs.StringVar(&o.ConfigMapName, "configmap-name", o.ConfigMapName, "name of configmap to write to")
	fs.StringVar(&o.LogInputs, "log-input", o.LogInputs, "jsonl file to read")
	fs.DurationVar(&o.Interval, "upload-interval", o.Interval, "frequency of read and upload cycles")
	fs.Int64Var(&o.NumberOfBytesToUpload, "content-size", o.NumberOfBytesToUpload, "number of bytes from the log that should be upload to the configmap every interval")
	fs.StringVar(&o.PodName, "pod-name", o.PodName, "Name of this pod: used to create a unique configmap key.")
}

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

func (o *MonitorUploaderOptions) Validate(_ context.Context) error {
	if len(o.KubeConfig) == 0 {
		return errors.New("missing required flag: --kubeconfig")
	}
	if len(o.LogInputs) == 0 {
		return errors.New("missing required flag: --log-input")
	}
	if len(o.ConfigMapNamespace) == 0 {
		return errors.New("missing required flag: --configmap-namespace")
	}
	if len(o.ConfigMapName) == 0 {
		return errors.New("missing required flag: --configmap-name")
	}
	if len(o.PodName) == 0 {
		return errors.New("missing required flag: --pod-name")
	}
	if o.NumberOfBytesToUpload == 0 {
		return errors.New("missing required flag: --content-size")
	}
	return nil
}

func (o *MonitorUploaderOptions) ToUploader(ctx context.Context) (*MonitorUploader, error) {
	timeoutContext, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	fmt.Printf("Waiting for kubeconfig: %v\n", o.KubeConfig)
	err := wait.PollUntilWithContext(timeoutContext, o.Interval, func(ctx context.Context) (bool, error) {
		_, err := os.Stat(o.KubeConfig)
		if os.IsNotExist(err) {
			fmt.Printf("Checking kubeconfig: %v, still not present.\n", o.KubeConfig)
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	fmt.Printf("Got kubeconfig: %v\n", o.KubeConfig)

	config, err := clientcmd.BuildConfigFromFlags("", o.KubeConfig)
	if err != nil {
		return nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &MonitorUploader{
		Interval:              o.Interval,
		LogInput:              o.LogInputs,
		NumberOfBytesToUpload: o.NumberOfBytesToUpload,
		ConfigMapNamespace:    o.ConfigMapNamespace,
		ConfigMapName:         o.ConfigMapName,
		ConfigMapClient:       kubeClient.CoreV1(),
		ConfigMapKey:          o.PodName,
		ErrorOut:              o.ErrorOut,
		FieldManager:          "monitor-uploader-" + o.PodName,
	}, nil
}

func (o *MonitorUploader) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	ctx, cancel := context.WithCancel(ctx)

	// handle teardown
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

	wait.UntilWithContext(ctx, o.ReadLastAndUpload, o.Interval)

	// upload one last time before we die.
	o.ReadLastAndUpload(context.TODO())

	return nil
}
func (o *MonitorUploader) ReadLastAndUpload(ctx context.Context) {
	if err := o.readLastAndUpload(ctx); err != nil {
		fmt.Fprintf(o.ErrorOut, "failed read and upload: %v\n", err)
	}
}

func (o *MonitorUploader) readLastAndUpload(ctx context.Context) error {
	fmt.Printf("Reading file: %v\n", o.LogInput)

	logFile, err := os.Open(o.LogInput)
	if err != nil {
		return err
	}
	defer logFile.Close()

	stat, err := logFile.Stat()
	if err != nil {
		return err
	}
	if fileSize := stat.Size(); fileSize > o.NumberOfBytesToUpload {
		startingOffset := fileSize - o.NumberOfBytesToUpload
		if _, err := logFile.Seek(startingOffset, io.SeekStart); err != nil {
			return err
		}
	}

	// this is not-so efficient way to find lines, BUT it gives a future chance to filter and we aren't reading that
	// much data anyway
	lastLines := []string{}
	lineReader := bufio.NewScanner(logFile)
	for lineReader.Scan() {
		currLine := lineReader.Text()
		// skip lines that are not in the expected jsonl format.  This is most likely to happen when we offset to the
		// end of a file and then start reading. We aren't doing anything to be sure we catch a line break.
		if !strings.HasPrefix(currLine, "{") {
			continue
		}
		lastLines = append(lastLines, currLine)
	}
	contentToWrite := strings.Join(lastLines, "\n")

	fmt.Printf("Uploading content\n")
	configMapToApply := applycorev1.ConfigMap(o.ConfigMapName, o.ConfigMapNamespace).WithData(
		map[string]string{
			o.ConfigMapKey: contentToWrite,
		},
	)
	if _, err := o.ConfigMapClient.ConfigMaps(o.ConfigMapNamespace).Apply(ctx, configMapToApply, metav1.ApplyOptions{FieldManager: o.FieldManager, Force: true}); err != nil {
		return err
	}

	return nil
}
