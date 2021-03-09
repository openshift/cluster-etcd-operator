package render

import (
	"bytes"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/ghodss/yaml"
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
}

func TestMain(m *testing.M) {
	// TODO: implement tests for bootstrap IP determination
	defaultBootstrapIPLocator = &fakeBootstrapIPLocator{ip: net.ParseIP("10.0.0.1")}
	os.Exit(m.Run())
}

func TestRenderIpv4(t *testing.T) {
	want := TemplateData{
		EtcdAddress: etcdAddress{
			LocalHost: "127.0.0.1",
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

	render := renderOpts{
		assetOutputDir:       dir,
		templateDir:          filepath.Join("../../..", "bindata", "bootkube"),
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
		EtcdAddress: etcdAddress{
			LocalHost: "127.0.0.1",
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
		EtcdAddress: etcdAddress{
			LocalHost: "127.0.0.1",
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
		EtcdAddress: etcdAddress{
			LocalHost: "[::1]",
		},
		ClusterCIDR:     []string{"10.128.0.0/14"},
		ServiceCIDR:     []string{"2001:db8::/32"},
		SingleStackIPv6: true,
	}

	config := &testConfig{
		t:                    t,
		clusterNetworkConfig: networkConfigIPv6SingleStack,
		infraConfig:          infraConfig,
		clusterConfigMap:     clusterConfigMap,
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

	render := &renderOpts{
		assetOutputDir:       dir,
		templateDir:          filepath.Join("../../..", "bindata", "bootkube"),
		errOut:               errOut,
		networkConfigFile:    clusterConfigFile.Name(),
		infraConfigFile:      infraConfigFile.Name(),
		clusterConfigMapFile: clusterConfigMapFile.Name(),
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
	case got.EtcdAddress.LocalHost != tc.want.EtcdAddress.LocalHost:
		tc.t.Errorf("LocalHost want: %q got: %q", tc.want.EtcdAddress.LocalHost, got.EtcdAddress.LocalHost)
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

const installConfigSingleStackIPv4 = `
apiVersion: v1
metadata:
  name: my-cluster
networking:
  clusterNetwork:
  - cidr: 10.128.0.0/14
    hostPrefix: 23
  machineCIDR: 10.0.0.0/16
  machineNetwork:
  - foo: bar
    cidr: 10.0.0.0/16
  networkType: OpenShiftSDN
  serviceNetwork:
  - 172.30.0.0/16
`

const installConfigDualStack = `
apiVersion: v1
metadata:
  name: my-cluster
networking:
  clusterNetwork:
  - cidr: 10.128.0.0/14
    hostPrefix: 23
  machineCIDR: 10.0.0.0/16
  machineNetwork:
  - foo: bar
    cidr: 2620:52:0:1302::/64
  - cidr: 10.0.0.0/16
  networkType: OpenShiftSDN
  serviceNetwork:
  - 172.30.0.0/16
`

const installConfigSingleStackIPv6 = `
apiVersion: v1
metadata:
  name: my-cluster
networking:
  clusterNetwork:
  - cidr: 10.128.0.0/14
    hostPrefix: 23
  machineCIDR: 10.0.0.0/16
  machineNetwork:
  - foo: bar
    cidr: 2620:52:0:1302::/64
  networkType: OpenShiftSDN
  serviceNetwork:
  - 172.30.0.0/16
`

func Test_getMachineCIDR(t *testing.T) {
	tests := map[string]struct {
		installConfig     string
		isSingleStackIPv6 bool
		expectedCIDR      string
		expectedErr       error
	}{
		"should locate the ipv4 cidr in a single stack ipv4 config": {
			installConfig:     installConfigSingleStackIPv4,
			isSingleStackIPv6: false,
			expectedCIDR:      "10.0.0.0/16",
			expectedErr:       nil,
		},
		"should locate the ipv4 cidr in a dual stack config": {
			installConfig:     installConfigDualStack,
			isSingleStackIPv6: false,
			expectedCIDR:      "10.0.0.0/16",
			expectedErr:       nil,
		},
		"should locate the ipv6 cidr in a single stack ipv6 config": {
			installConfig:     installConfigSingleStackIPv6,
			isSingleStackIPv6: true,
			expectedCIDR:      "2620:52:0:1302::/64",
			expectedErr:       nil,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var installConfig map[string]interface{}
			if err := yaml.Unmarshal([]byte(test.installConfig), &installConfig); err != nil {
				panic(err)
			}
			cidr, err := getMachineCIDR(installConfig, test.isSingleStackIPv6)
			if err != nil {
				if test.expectedErr == nil {
					t.Errorf("unexpected error: %w", err)
					return
				}
			} else {
				if test.expectedErr != nil {
					t.Errorf("expected but didn't get an error")
				}
			}
			if cidr != test.expectedCIDR {
				t.Errorf("expected CIDR %q, got %q", test.expectedCIDR, cidr)
			}
		})
	}
}
