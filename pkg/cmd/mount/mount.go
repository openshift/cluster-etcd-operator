package mount

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"path"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/klog"

	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type mountSecretOpts struct {
	errOut     io.Writer
	commonName string
	assetsDir  string
}

// NewMountCommand creates a staticsync controller.
func NewMountCommand(errOut io.Writer) *cobra.Command {
	mountSecretOpts := &mountSecretOpts{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "mount --FLAGS",
		Short: "Mount a secret with certs",
		Long:  "This command mounts the secret with valid certs signed by etcd-cert-signer-controller",
		Run: func(cmd *cobra.Command, args []string) {
			if err := mountSecretOpts.validateMountSecretOpts(); err != nil {
				if cmd.HasParent() {
					klog.Fatal(err)
				}
				fmt.Fprint(mountSecretOpts.errOut, err.Error())
			}
			if err := mountSecretOpts.Run(context.TODO()); err != nil {
				if cmd.HasParent() {
					klog.Fatal(err)
				}
				fmt.Fprint(mountSecretOpts.errOut, err.Error())
			}
		},
	}

	mountSecretOpts.AddFlags(cmd.Flags())
	return cmd
}

func (m *mountSecretOpts) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&m.commonName, "commonname", "", "Common name for the certificate being requested")
	fs.StringVar(&m.assetsDir, "assetsdir", "", "Directory location to store signed certs")
}

func (m *mountSecretOpts) Run(ctx context.Context) error {
	return m.mountSecret(ctx)
}

func (m *mountSecretOpts) validateMountSecretOpts() error {
	if m.commonName == "" {
		return fmt.Errorf("missing required flag: --commonname")
	}
	if m.assetsDir == "" {
		return fmt.Errorf("missing required flag: --assetsdir")
	}
	return nil

}

// mount will secret will look for secret in the form of
// <profile>-<podFQDN>, where profile can be peer, server
// and metric and mount the certs as commonname.crt/commonname.key
// this will run as init container in etcd pod managed by CEO.
func (m *mountSecretOpts) mountSecret(ctx context.Context) error {
	var err error
	inClusterConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("error creating in cluster client config: %v", err)
	}

	client, err := kubernetes.NewForConfig(inClusterConfig)
	if err != nil {
		return fmt.Errorf("error creating client: %v", err)
	}

	duration := 10 * time.Second
	var s *v1.Secret
	// wait forever for success and retry every duration interval
	err = wait.PollInfinite(duration, func() (bool, error) {
		s, err = client.CoreV1().Secrets("openshift-etcd").Get(ctx, getSecretName(m.commonName), metav1.GetOptions{})
		if err != nil {
			klog.Errorf("error in getting secret %s/%s: %v", "openshift-etcd", getSecretName(m.commonName), err)
			return false, err
		}
		err = ensureCertKeys(s.Data)
		if err != nil {
			return false, err
		}

		return true, nil

	})

	if err != nil {
		return err
	}

	// write out signed certificate to disk
	certFile := path.Join(m.assetsDir, m.commonName+".crt")
	if err := ioutil.WriteFile(certFile, s.Data["tls.crt"], 0644); err != nil {
		return fmt.Errorf("unable to write to %s: %v", certFile, err)
	}
	keyFile := path.Join(m.assetsDir, m.commonName+".key")
	if err := ioutil.WriteFile(keyFile, s.Data["tls.key"], 0644); err != nil {
		return fmt.Errorf("unable to write to %s: %v", keyFile, err)
	}
	return nil
}

func getSecretName(commonName string) string {
	prefix := ""
	if strings.Contains(commonName, "peer") {
		prefix = "peer"
	}
	if strings.Contains(commonName, "server") {
		prefix = "server"
	}
	if strings.Contains(commonName, "metric") {
		prefix = "metric"
	}
	return prefix + "-" + strings.Split(commonName, ":")[2]
}

func ensureCertKeys(data map[string][]byte) error {
	if len(data["tls.crt"]) == 0 || len(data["tls.key"]) == 0 {
		return fmt.Errorf("invalid secret data")
	}
	return nil
}
