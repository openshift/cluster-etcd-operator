package render

import (
	"bytes"
	"io"
	"io/ioutil"
	"net"
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
	infraConfig = `
apiVersion: config.openshift.io/v1
kind: Infrastructure
metadata:
  name: cluster
spec:
  cloudConfig:
    name: ""
status:
  platform: AWS
  platformStatus:
    aws:
      region: us-east-1
    type: AWS
`

	clusterConfigMap = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-config-v1
  namespace: kube-system
data:
  install-config: |
    apiVersion: v1
    baseDomain: gcp.devcluster.openshift.com
    compute:
    - architecture: amd64
      hyperthreading: Enabled
      name: worker
      platform: {}
      replicas: 3
    controlPlane:
      architecture: amd64
      hyperthreading: Enabled
      name: master
      platform:
        gcp:
          osDisk:
            DiskSizeGB: 128
            DiskType: pd-ssd
          type: n1-standard-4
          zones:
          - us-east1-b
          - us-east1-c
          - us-east1-d
      replicas: 3
    metadata:
      creationTimestamp: null
      name: my-cluster
    networking:
      clusterNetwork:
      - cidr: 10.128.0.0/14
        hostPrefix: 23
      machineCIDR: 10.0.0.0/16
      machineNetwork:
      - cidr: 10.0.0.0/16
      networkType: OpenShiftSDN
      serviceNetwork:
      - 172.30.0.0/16
    platform:
      gcp:
        projectID: openshift
        region: us-east1
    publish: External
`
)

type testConfig struct {
	t                    *testing.T
	clusterNetworkConfig string
	infraConfig          string
	clusterConfigMap     string
	want                 TemplateData
	bootstrapIP          string
}

func TestMain(m *testing.M) {
	// TODO: implement tests for bootstrap IP determination
	defaultBootstrapIPLocator = &fakeBootstrapIPLocator{ip: net.ParseIP("10.0.0.1")}
	os.Exit(m.Run())
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
		infraConfig:          infraConfig,
		clusterConfigMap:     clusterConfigMap,
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

	infraConfigFile, err := ioutil.TempFile(dir, "cluster-infrastructure-02-config.*.yaml")
	if err != nil {
		tc.t.Fatal(err)
	}
	defer infraConfigFile.Close()

	clusterConfigMapFile, err := ioutil.TempFile(dir, "cluster-config.*.yaml")
	if err != nil {
		tc.t.Fatal(err)
	}
	defer clusterConfigMapFile.Close()

	if err := writeFile(tc.clusterNetworkConfig, clusterConfigFile); err != nil {
		tc.t.Fatal(err)
	}

	if err := writeFile(tc.infraConfig, infraConfigFile); err != nil {
		tc.t.Fatal(err)
	}

	if err := writeFile(tc.clusterConfigMap, clusterConfigMapFile); err != nil {
		tc.t.Fatal(err)
	}

	generic := options.GenericOptions{
		AssetInputDir:    dir,
		AssetOutputDir:   dir,
		TemplatesDir:     filepath.Join("../../..", "bindata", "bootkube"),
		ConfigOutputFile: filepath.Join(dir, "config"),
	}

	render := renderOpts{
		generic:              generic,
		manifest:             *options.NewManifestOptions("etcd"),
		errOut:               errOut,
		networkConfigFile:    clusterConfigFile.Name(),
		infraConfigFile:      infraConfigFile.Name(),
		clusterConfigMapFile: clusterConfigMapFile.Name(),
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
		infraConfig:          infraConfig,
		clusterConfigMap:     clusterConfigMap,
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
		infraConfig:          infraConfig,
		clusterConfigMap:     clusterConfigMap,
		want:                 want,
	}
	testTemplateData(config)
}

func TestTemplateDataSingleStack(t *testing.T) {
	want := TemplateData{
		ManifestConfig: options.ManifestConfig{
			EtcdAddress: options.EtcdAddress{
				LocalHost: "[::1]",
			},
		},
		ClusterCIDR:     []string{"10.128.0.0/14"},
		ServiceCIDR:     []string{"2001:db8::/32"},
		SingleStackIPv6: true,
		BootstrapIP:     "2001:0DB8:C21A",
	}

	config := &testConfig{
		t:                    t,
		clusterNetworkConfig: networkConfigIPv6SingleStack,
		infraConfig:          infraConfig,
		clusterConfigMap:     clusterConfigMap,
		want:                 want,
		bootstrapIP:          "2001:0DB8:C21A",
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

	infraConfigFile, err := ioutil.TempFile(dir, "cluster-infrastructures-02-config.*.yaml")
	if err != nil {
		tc.t.Fatal(err)
	}
	defer infraConfigFile.Close()

	clusterConfigMapFile, err := ioutil.TempFile(dir, "cluster-config.*.yaml")
	if err != nil {
		tc.t.Fatal(err)
	}
	defer clusterConfigMapFile.Close()

	if err := writeFile(tc.clusterNetworkConfig, clusterConfigFile); err != nil {
		tc.t.Fatal(err)
	}

	if err := writeFile(tc.infraConfig, infraConfigFile); err != nil {
		tc.t.Fatal(err)
	}

	if err := writeFile(tc.clusterConfigMap, clusterConfigMapFile); err != nil {
		tc.t.Fatal(err)
	}

	generic := options.GenericOptions{
		AssetInputDir:    dir,
		AssetOutputDir:   dir,
		TemplatesDir:     filepath.Join("../../..", "bindata", "bootkube"),
		ConfigOutputFile: filepath.Join(dir, "config"),
	}

	render := &renderOpts{
		generic:              generic,
		manifest:             *options.NewManifestOptions("etcd"),
		errOut:               errOut,
		networkConfigFile:    clusterConfigFile.Name(),
		infraConfigFile:      infraConfigFile.Name(),
		clusterConfigMapFile: clusterConfigMapFile.Name(),
		bootstrapIP:          tc.bootstrapIP,
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
	case got.BootstrapIP != tc.want.BootstrapIP:
		// if we don't say we want a specific IP we dont fail.
		if tc.want.BootstrapIP != "" {
			tc.t.Errorf("BootstrapIP want: %q got: %q", tc.want.BootstrapIP, got.BootstrapIP)
		}
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

type fakeBootstrapIPLocator struct {
	ip net.IP
}

func (f *fakeBootstrapIPLocator) getBootstrapIP(ipv6 bool, machineCIDR string, excludedIPs []string) (net.IP, error) {
	return f.ip, nil
}
