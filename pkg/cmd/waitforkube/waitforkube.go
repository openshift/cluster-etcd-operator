package waitforkube

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
)

const (
	retryDuration = 10 * time.Second
	saTokenPath   = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	dataDir       = "/var/lib/etcd" //TODO should be flag or ENV
	dbPath        = "member/snap/db"
)

type waitForKube struct {
	errOut io.Writer
}

// NewWaitForKubeCommand waits for kube to come up before continuing to start the rest of the containers.
func NewWaitForKubeCommand(errOut io.Writer) *cobra.Command {
	waitForKube := &waitForKube{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "wait-for-kube",
		Short: "wait for kube service account to exist",
		Long:  "This command makes sure that the kube is available before starting the rest of the containers in the pod.",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
						fmt.Fprint(waitForKube.errOut, err.Error())
					}
				}
			}
			must(waitForKube.Run)
		},
	}

	return cmd
}

func (w *waitForKube) Run() error {
	dataFilePath := filepath.Join(dataDir, dbPath)
	if CheckEtcdDataFileExists(dataFilePath) {
		klog.Infof("etcd has previously been initialized as a member")
		return nil
	}
	wait.PollInfinite(retryDuration, func() (bool, error) {
		if _, err := os.Stat(saTokenPath); os.IsNotExist(err) {
			klog.Infof("waiting for kube service account resources to sync: %v", err)
			return false, nil
		}
		klog.Infof("kube dependencies verified, service account resources found")
		return true, nil
	})
	if !inCluster() {
		return fmt.Errorf("container missing required env KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT")
	}
	return nil
}

// CheckEtcdDataFileExists checks for the existance of an etcd db file. If the db exists we can assume this etcd
// has previously started or is currently being recovered. If true we can not assume kube exists yet or will
// during the life of this container. For example disaster recovery.
// TODO organize into a general util
func CheckEtcdDataFileExists(dataFilePath string) bool {
	if _, err := os.Stat(dataFilePath); err != nil {
		if os.IsNotExist(err) {
			klog.Infof("CheckEtcdDataFileExists(): %s does not exist, waiting for kube", dataFilePath)
			return false
		}
		// print error for debug but technically we can't say the file does not exist
		klog.Errorf("CheckEtcdDataFileExists(): %v", err)
	}
	return true
}

//TODO: add to util
func inCluster() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" || os.Getenv("KUBERNETES_SERVICE_PORT") == "" {
		return false
	}
	return true
}
