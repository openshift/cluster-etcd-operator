package waitforkube

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
)

const (
	retryDuration = 10 * time.Second
	saTokenPath   = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

type waitForKubeOpts struct {
	errOut io.Writer
}

// NewWaitForKubeCommand waits for kube to come up before continuing to start the rest of the containers.
func NewWaitForKubeCommand(errOut io.Writer) *cobra.Command {
	waitForKubeOpts := &waitForKubeOpts{
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
						fmt.Fprint(waitForKubeOpts.errOut, err.Error())
					}
				}
			}
			must(waitForKubeOpts.Run)
		},
	}

	return cmd
}

func (w *waitForKubeOpts) Run() error {
	wait.PollInfinite(retryDuration, func() (bool, error) {
		if _, err := os.Stat(saTokenPath); os.IsNotExist(err) {
			klog.Infof("waiting for kube service account resources to sync: %v", err)
			return false, nil
		}
		return true, nil
	})
	if !inCluster() {
		return fmt.Errorf("kube env not populated")
	}
	return nil
}

//TODO: add to util
func inCluster() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" || os.Getenv("KUBERNETES_SERVICE_PORT") == "" {
		return false
	}
	return true
}
