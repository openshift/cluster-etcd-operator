package aio

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"

	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog"
)

const (
	aioNodeName       = "aio"
	aioNodeInternalIP = "127.0.0.1"
)

// aioOpts holds values to drive the aio command.
type aioOpts struct {
	errOut io.Writer

	etcdCACert       string
	etcdCAKey        string
	etcdMetricCACert string
	etcdMetricCAKey  string
	assetOutputDir   string
}

// NewAIOCommand creates a all-in-one render command.
func NewAIOCommand(errOut io.Writer) *cobra.Command {
	aioOpts := aioOpts{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "aio",
		Short: "Render all-in-one etcd manifests and related resources",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(aioOpts.errOut, err.Error())
				}
			}

			must(aioOpts.Validate)
			must(aioOpts.Run)
		},
	}

	aioOpts.AddFlags(cmd.Flags())

	return cmd
}

func (a *aioOpts) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&a.etcdCACert, "etcd-ca-cert", a.etcdCACert, "path to etcd CA certificate")
	fs.StringVar(&a.etcdCAKey, "etcd-ca-key", a.etcdCAKey, "path to etcd CA key")
	fs.StringVar(&a.etcdMetricCACert, "etcd-metric-ca-cert", a.etcdMetricCACert, "path to etcd metric CA certificate")
	fs.StringVar(&a.etcdMetricCAKey, "etcd-metric-ca-key", a.etcdMetricCAKey, "path to etcd metric CA key")
	fs.StringVar(&a.assetOutputDir, "asset-output-dir", a.assetOutputDir, "path for rendered assets")
}

// Validate verifies the inputs.
func (a *aioOpts) Validate() error {
	if len(a.etcdCACert) == 0 {
		return errors.New("missing required flag: --etcd-ca-cert")
	}
	if len(a.etcdCAKey) == 0 {
		return errors.New("missing required flag: --etcd-ca-key")
	}
	if len(a.etcdMetricCACert) == 0 {
		return errors.New("missing required flag: --etcd-metric-ca-cert")
	}
	if len(a.etcdMetricCAKey) == 0 {
		return errors.New("missing required flag: --etcd-metric-ca-key")
	}
	if len(a.assetOutputDir) == 0 {
		return errors.New("missing required flag: --asset-output-dir")
	}
	return nil
}

// Run contains the logic of the aio command.
func (a *aioOpts) Run() error {
	err := a.generateEtcdNodeCerts(aioNodeName, aioNodeInternalIP)
	if err != nil {
		return err
	}
	return nil
}

func (a *aioOpts) generateEtcdNodeCerts(nodeName, nodeInternalIP string) error {
	caCertData, err := ioutil.ReadFile(a.etcdCACert)
	if err != nil {
		return fmt.Errorf("Failed to read --etcd-ca-cert file: %s", err)
	}

	caKeyData, err := ioutil.ReadFile(a.etcdCAKey)
	if err != nil {
		return fmt.Errorf("Failed to read --etcd-ca-key file: %s", err)
	}

	metricCACertData, err := ioutil.ReadFile(a.etcdMetricCACert)
	if err != nil {
		return fmt.Errorf("Failed to read --etcd-metric-ca-cert file: %s", err)
	}

	metricCAKeyData, err := ioutil.ReadFile(a.etcdMetricCAKey)
	if err != nil {
		return fmt.Errorf("Failed to read --etcd-metric-ca-key file: %s", err)
	}

	nodeInternalIPs := []string{nodeInternalIP}

	certData, keyData, err := tlshelpers.CreateServerCertKey(caCertData, caKeyData, nodeInternalIPs)
	if err != nil {
		return err
	}
	err = a.writeCertKeyFiles(tlshelpers.EtcdAllServingSecretName, tlshelpers.GetServingSecretNameForNode(nodeName), certData, keyData)
	if err != nil {
		return err
	}

	certData, keyData, err = tlshelpers.CreatePeerCertKey(caCertData, caKeyData, nodeInternalIPs)
	if err != nil {
		return err
	}
	err = a.writeCertKeyFiles(tlshelpers.EtcdAllPeerSecretName, tlshelpers.GetPeerClientSecretNameForNode(nodeName), certData, keyData)
	if err != nil {
		return err
	}

	certData, keyData, err = tlshelpers.CreateMetricCertKey(metricCACertData, metricCAKeyData, nodeInternalIPs)
	if err != nil {
		return err
	}
	err = a.writeCertKeyFiles(tlshelpers.EtcdAllServingMetricsSecretName, tlshelpers.GetServingMetricsSecretNameForNode(nodeName), certData, keyData)
	if err != nil {
		return err
	}

	return nil
}

func (a *aioOpts) writeCertKeyFiles(allSecretName, nodeSecretName string, certData, keyData *bytes.Buffer) error {
	dir := path.Join(a.assetOutputDir, "secrets", allSecretName)

	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return fmt.Errorf("Failed to create %s directory: %s", allSecretName, err)
	}

	err = ioutil.WriteFile(path.Join(dir, nodeSecretName+".crt"), certData.Bytes(), 0600)
	if err != nil {
		return fmt.Errorf("Failed to write %s cert: %s", allSecretName, err)
	}
	err = ioutil.WriteFile(path.Join(dir, nodeSecretName+".key"), keyData.Bytes(), 0600)
	if err != nil {
		return fmt.Errorf("Failed to write %s key: %s", allSecretName, err)
	}

	return nil
}
