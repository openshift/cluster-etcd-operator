package render

import (
	"bytes"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/render/options"
)

var (
	expectedServiceIPv4CIDR        = []string{"172.30.0.0/16"}
	expectedServiceMixedCIDR       = []string{"172.30.0.0/16", "2001:db8::/32"}
	expectedServiceMixedSwapCIDR   = []string{"2001:db8::/32", "172.30.0.0/16"}
	expectedServiceSingleStackCIDR = []string{"2001:db8::/32"}

	clusterAPIConfig = `
apiVersion: machine.openshift.io/v1beta1
kind: Cluster
metadata:
  creationTimestamp: null
  name: cluster
  namespace: openshift-machine-api
spec:
  clusterNetwork:
    pods:
      cidrBlocks:
      - 2001:db8::/32
    serviceDomain: ""
    services:
      cidrBlocks:
        - 172.30.0.0/16
  providerSpec: {}
status: {}
`
	networkConfigIpv4 = `
apiVersion: config.openshift.io/v1
kind: Network
metadata:
  creationTimestamp: null
  name: cluster
spec:
  clusterNetwork:
    - cidr: 10.128.0.0/14
      hostPrefix: 23
  networkType: OpenShiftSDN
  serviceNetwork:
    - 172.30.0.0/16
status: {}
`
	networkConfigMixed = `
apiVersion: config.openshift.io/v1
kind: Network
metadata:
  creationTimestamp: null
  name: cluster
spec:
  clusterNetwork:
    - cidr: 10.128.0.0/14
      hostPrefix: 23
  networkType: OpenShiftSDN
  serviceNetwork:
    - 172.30.0.0/16
    - 2001:db8::/32
status: {}
`
	networkConfigMixedSwap = `
apiVersion: config.openshift.io/v1
kind: Network
metadata:
  creationTimestamp: null
  name: cluster
spec:
  clusterNetwork:
    - cidr: 10.128.10.0/14
      hostPrefix: 23
  networkType: OpenShiftSDN
  serviceNetwork:
    - 2001:db8::/32
    - 172.30.0.0/16
status: {}
`
	networkConfigIPv6SingleStack = `
apiVersion: config.openshift.io/v1
kind: Network
metadata:
  creationTimestamp: null
  name: cluster
spec:
  clusterNetwork:
    - cidr: 10.128.0.0/14
      hostPrefix: 23
  networkType: OpenShiftSDN
  serviceNetwork:
    - 2001:db8::/32
status: {}
`
)

type testConfig struct {
	t                    *testing.T
	clusterNetworkConfig string
	want                 TemplateData
}

func TestRenderIpv4(t *testing.T) {
	want := TemplateData{
		ManifestConfig: options.ManifestConfig{
			EtcdAddress: options.EtcdAddress{
				LocalHost: "127.0.0.1",
			},
		},
		ClusterCIDR:     []string{"10.128.0.0/14"},
		ServiceCIDR:     []string{"172.30.0.0/16"},
		SingleStackIPv6: false,
	}

	config := &testConfig{
		t:                    t,
		clusterNetworkConfig: networkConfigIpv4,
		want:                 want,
	}

	testRender(config)
}

func testRender(tc *testConfig) {
	var errOut io.Writer
	dir, err := ioutil.TempDir("/tmp", "assets-")
	if err != nil {
		tc.t.Fatal(err)
	}

	defer os.RemoveAll(dir) // clean up

	clusterConfigFile, err := ioutil.TempFile(dir, "cluster-network-02-config.*.yaml")
	if err != nil {
		tc.t.Fatal(err)
	}
	defer clusterConfigFile.Close()

	if err := writeFile(tc.clusterNetworkConfig, clusterConfigFile); err != nil {
		tc.t.Fatal(err)
	}

	generic := options.GenericOptions{
		AssetInputDir:    dir,
		AssetOutputDir:   dir,
		TemplatesDir:     filepath.Join("../../..", "bindata", "bootkube"),
		ConfigOutputFile: filepath.Join(dir, "config"),
	}

	render := renderOpts{
		generic:           generic,
		manifest:          *options.NewManifestOptions("etcd"),
		errOut:            errOut,
		clusterConfigFile: clusterConfigFile.Name(),
	}

	if err := render.Run(); err != nil {
		tc.t.Errorf("failed render.Run(): %v", err)
	}
}

func TestTemplateDataIpv4(t *testing.T) {
	want := TemplateData{
		ManifestConfig: options.ManifestConfig{
			EtcdAddress: options.EtcdAddress{
				LocalHost: "127.0.0.1",
			},
		},
		ClusterCIDR:     []string{"10.128.0.0/14"},
		ServiceCIDR:     []string{"172.30.0.0/16"},
		SingleStackIPv6: false,
	}

	config := &testConfig{
		t:                    t,
		clusterNetworkConfig: networkConfigIpv4,
		want:                 want,
	}
	testTemplateData(config)
}

func TestTemplateDataMixed(t *testing.T) {
	want := TemplateData{
		ManifestConfig: options.ManifestConfig{
			EtcdAddress: options.EtcdAddress{
				LocalHost: "127.0.0.1",
			},
		},
		ClusterCIDR:     []string{"10.128.10.0/14"},
		ServiceCIDR:     []string{"2001:db8::/32", "172.30.0.0/16"},
		SingleStackIPv6: false,
	}

	config := &testConfig{
		t:                    t,
		clusterNetworkConfig: networkConfigMixedSwap,
		want:                 want,
	}
	testTemplateData(config)
}

func TestTemplateDataSingleStack(t *testing.T) {
	want := TemplateData{
		ManifestConfig: options.ManifestConfig{
			EtcdAddress: options.EtcdAddress{
				LocalHost: "::1",
			},
		},
		ClusterCIDR:     []string{"10.128.0.0/14"},
		ServiceCIDR:     []string{"2001:db8::/32"},
		SingleStackIPv6: true,
	}

	config := &testConfig{
		t:                    t,
		clusterNetworkConfig: networkConfigIPv6SingleStack,
		want:                 want,
	}
	testTemplateData(config)
}

func testTemplateData(tc *testConfig) {
	var errOut io.Writer
	dir, err := ioutil.TempDir("/tmp", "assets-")
	if err != nil {
		tc.t.Fatal(err)
	}
	defer os.RemoveAll(dir) // clean up

	clusterConfigFile, err := ioutil.TempFile(dir, "cluster-network-02-config.*.yaml")
	if err != nil {
		tc.t.Fatal(err)
	}
	defer clusterConfigFile.Close()

	if err := writeFile(tc.clusterNetworkConfig, clusterConfigFile); err != nil {
		tc.t.Fatal(err)
	}

	generic := options.GenericOptions{
		AssetInputDir:    dir,
		AssetOutputDir:   dir,
		TemplatesDir:     filepath.Join("../../..", "bindata", "bootkube"),
		ConfigOutputFile: filepath.Join(dir, "config"),
	}

	render := &renderOpts{
		generic:           generic,
		manifest:          *options.NewManifestOptions("etcd"),
		errOut:            errOut,
		clusterConfigFile: clusterConfigFile.Name(),
	}

	got, err := newTemplateData(render)
	if err != nil {
		tc.t.Fatal(err)
	}

	switch {
	case got.ClusterCIDR[0] != tc.want.ClusterCIDR[0]:
		tc.t.Errorf("ClusterCIDR[0] want: %q got: %q", tc.want.ClusterCIDR[0], got.ClusterCIDR[0])
	case len(got.ClusterCIDR) != len(tc.want.ClusterCIDR):
		tc.t.Errorf("len(ClusterCIDR) want: %d got: %d", len(tc.want.ClusterCIDR), len(got.ClusterCIDR))
	case got.ServiceCIDR[0] != tc.want.ServiceCIDR[0]:
		tc.t.Errorf("ServiceCIDR[0] want: %q got: %q", tc.want.ServiceCIDR[0], got.ServiceCIDR[0])
	case len(got.ServiceCIDR) != len(tc.want.ServiceCIDR):
		tc.t.Errorf("len(ServiceCIDR) want: %d got: %d", len(tc.want.ServiceCIDR), len(got.ServiceCIDR))
	case got.SingleStackIPv6 != tc.want.SingleStackIPv6:
		tc.t.Errorf("SingleStackIPv6 want: %v got: %v", tc.want.SingleStackIPv6, got.SingleStackIPv6)
	case got.ManifestConfig.EtcdAddress.LocalHost != tc.want.ManifestConfig.EtcdAddress.LocalHost:
		tc.t.Errorf("LocalHost want: %q got: %q", tc.want.ManifestConfig.EtcdAddress.LocalHost, got.ManifestConfig.EtcdAddress.LocalHost)
	}
}

func writeFile(input string, w io.Writer) error {
	var buffer bytes.Buffer
	buffer.WriteString(input)
	if _, err := buffer.WriteTo(w); err != nil {
		return err
	}
	return nil
}
