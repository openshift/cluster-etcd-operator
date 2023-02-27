package render

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ghodss/yaml"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
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
	clusterConfigMapSingleMaster = `
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
      name: worker
      platform: {}
      replicas: 3
    controlPlane:
      name: master
      platform:
        gcp:
      replicas: 1
    metadata:
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
    bootstrapInPlace:
      installationDisk: /dev/sda
`
)

type testConfig struct {
	t                                       *testing.T
	clusterNetworkConfig                    string
	infraConfig                             string
	clusterConfigMap                        string
	delayedHABootstrapScalingStrategyMarker string
	want                                    TemplateData
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
		ClusterCIDR: []string{"10.128.0.0/14"},
		ServiceCIDR: []string{"172.30.0.0/16"},
		PreferIPv6:  false,
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
		ClusterCIDR: []string{"10.128.0.0/14"},
		ServiceCIDR: []string{"172.30.0.0/16"},
		PreferIPv6:  false,
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

func TestRenderScalingStrategyBootstrapInPlace(t *testing.T) {
	want := TemplateData{
		BootstrapScalingStrategy:  ceohelpers.BootstrapInPlaceStrategy,
		EtcdEndpointConfigmapData: "MTAuMC4wLjE: 10.0.0.1",
	}

	config := &testConfig{
		t:                    t,
		clusterNetworkConfig: networkConfigIpv4,
		infraConfig:          infraConfig,
		clusterConfigMap:     clusterConfigMapSingleMaster,
		want:                 want,
	}

	testTemplateData(config)
}

func TestRenderScalingStrategyDelayedHA(t *testing.T) {
	want := TemplateData{
		BootstrapScalingStrategy: ceohelpers.DelayedHAScalingStrategy,
		NamespaceAnnotations:     map[string]string{ceohelpers.DelayedHABootstrapScalingStrategyAnnotation: ""},
	}
	config := &testConfig{
		t:                                       t,
		clusterNetworkConfig:                    networkConfigIpv4,
		infraConfig:                             infraConfig,
		clusterConfigMap:                        clusterConfigMap,
		delayedHABootstrapScalingStrategyMarker: "/dev/null", // exists
		want:                                    want,
	}

	testTemplateData(config)
}

func TestTemplateDataMixed(t *testing.T) {
	want := TemplateData{
		EtcdAddress: etcdAddress{
			LocalHost: "[::1]",
		},
		ClusterCIDR: []string{"10.128.10.0/14"},
		ServiceCIDR: []string{"2001:db8::/32", "172.30.0.0/16"},
		PreferIPv6:  true,
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
		ClusterCIDR: []string{"10.128.0.0/14"},
		ServiceCIDR: []string{"2001:db8::/32"},
		PreferIPv6:  true,
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
		assetOutputDir:                          dir,
		templateDir:                             filepath.Join("../../..", "bindata", "bootkube"),
		errOut:                                  errOut,
		networkConfigFile:                       clusterConfigFile.Name(),
		infraConfigFile:                         infraConfigFile.Name(),
		clusterConfigMapFile:                    clusterConfigMapFile.Name(),
		delayedHABootstrapScalingStrategyMarker: tc.delayedHABootstrapScalingStrategyMarker,
	}

	got, err := newTemplateData(render)
	if err != nil {
		tc.t.Fatal(err)
	}

	//TODO make more readable possibly as []func
	switch {
	case tc.want.ClusterCIDR != nil && got.ClusterCIDR[0] != tc.want.ClusterCIDR[0]:
		tc.t.Errorf("ClusterCIDR[0] want: %q got: %q", tc.want.ClusterCIDR[0], got.ClusterCIDR[0])
	case tc.want.ClusterCIDR != nil && len(got.ClusterCIDR) != len(tc.want.ClusterCIDR):
		tc.t.Errorf("len(ClusterCIDR) want: %d got: %d", len(tc.want.ClusterCIDR), len(got.ClusterCIDR))
	case tc.want.ServiceCIDR != nil && got.ServiceCIDR[0] != tc.want.ServiceCIDR[0]:
		tc.t.Errorf("ServiceCIDR[0] want: %q got: %q", tc.want.ServiceCIDR[0], got.ServiceCIDR[0])
	case tc.want.ServiceCIDR != nil && len(got.ServiceCIDR) != len(tc.want.ServiceCIDR):
		tc.t.Errorf("len(ServiceCIDR) want: %d got: %d", len(tc.want.ServiceCIDR), len(got.ServiceCIDR))
	case got.PreferIPv6 != tc.want.PreferIPv6:
		tc.t.Errorf("PreferIPv6 want: %v got: %v", tc.want.PreferIPv6, got.PreferIPv6)
	case tc.want.EtcdAddress.LocalHost != "" && got.EtcdAddress.LocalHost != tc.want.EtcdAddress.LocalHost:
		tc.t.Errorf("LocalHost want: %q got: %q", tc.want.EtcdAddress.LocalHost, got.EtcdAddress.LocalHost)
	case tc.want.EtcdEndpointConfigmapData != "" && got.EtcdEndpointConfigmapData != tc.want.EtcdEndpointConfigmapData:
		tc.t.Errorf("EtcdEndpointConfigmapData want: %q got: %q", tc.want.EtcdEndpointConfigmapData, got.EtcdEndpointConfigmapData)
	case tc.want.BootstrapScalingStrategy != "" && got.BootstrapScalingStrategy != tc.want.BootstrapScalingStrategy:
		tc.t.Errorf("BootstrapScalingStrategy want: %q got: %q", tc.want.BootstrapScalingStrategy, got.BootstrapScalingStrategy)
	case tc.want.NamespaceAnnotations != nil && !reflect.DeepEqual(got.NamespaceAnnotations, tc.want.NamespaceAnnotations):
		tc.t.Errorf("NamespaceAnnotations want: %q got: %q", tc.want.NamespaceAnnotations, got.NamespaceAnnotations)
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

const installConfigDualStackIPv6Primary = `
apiVersion: v1
metadata:
  name: my-cluster
networking:
  clusterNetwork:
  - cidr: 10.128.0.0/14
    hostPrefix: 23
  machineCIDR: 10.0.0.0/16
  machineNetwork:
  - cidr: 2620:52:0:1302::/64
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

const installConfigReservedIPv4CIDR = `
apiVersion: v1
metadata:
  name: my-cluster
networking:
  clusterNetwork:
  - cidr: 10.128.0.0/14
    hostPrefix: 23
  machineCIDR: 192.0.2.0/24
  machineNetwork:
  - foo: bar
    cidr: 192.0.2.0/24
  networkType: OpenShiftSDN
  serviceNetwork:
  - 172.30.0.0/16
`
const installConfigReservedIPv6CIDR = `
apiVersion: v1
metadata:
  name: my-cluster
networking:
  clusterNetwork:
  - cidr: 10.128.0.0/14
    hostPrefix: 23
  machineCIDR: 2001:db8::/32
  machineNetwork:
  - foo: bar
    cidr: 2001:db8::/32
  networkType: OpenShiftSDN
  serviceNetwork:
  - 172.30.0.0/16
`

func Test_getMachineCIDR(t *testing.T) {
	tests := map[string]struct {
		installConfig string
		preferIPv6    bool
		expectedCIDR  string
		expectedErr   error
	}{
		"should locate the ipv4 cidr in a single stack ipv4 config": {
			installConfig: installConfigSingleStackIPv4,
			preferIPv6:    false,
			expectedCIDR:  "10.0.0.0/16",
			expectedErr:   nil,
		},
		"should locate the ipv4 cidr in a dual stack config": {
			installConfig: installConfigDualStack,
			preferIPv6:    false,
			expectedCIDR:  "10.0.0.0/16",
			expectedErr:   nil,
		},
		"should locate the ipv6 cidr in a v6-primary dual stack config": {
			installConfig: installConfigDualStack,
			preferIPv6:    true,
			expectedCIDR:  "2620:52:0:1302::/64",
			expectedErr:   nil,
		},
		"should locate the ipv6 cidr in a single stack ipv6 config": {
			installConfig: installConfigSingleStackIPv6,
			preferIPv6:    true,
			expectedCIDR:  "2620:52:0:1302::/64",
			expectedErr:   nil,
		},
		"should error on a reserved ipv4 cidr": {
			installConfig: installConfigReservedIPv4CIDR,
			preferIPv6:    false,
			expectedCIDR:  "",
			expectedErr:   fmt.Errorf("machineNetwork CIDR is reserved and unsupported: \"192.0.2.0\""),
		},
		"should error on a reserved ipv6 cidr": {
			installConfig: installConfigReservedIPv6CIDR,
			preferIPv6:    true,
			expectedCIDR:  "",
			expectedErr:   fmt.Errorf("machineNetwork CIDR is reserved and unsupported: \"2001:db8::\""),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var installConfig map[string]interface{}
			if err := yaml.Unmarshal([]byte(test.installConfig), &installConfig); err != nil {
				panic(err)
			}
			cidr, err := getMachineCIDR(installConfig, test.preferIPv6)
			if err == nil && test.expectedErr != nil {
				t.Fatalf("didn't get an error, expected: %v", test.expectedErr)
			}

			if err != nil && test.expectedErr == nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if err != nil && test.expectedErr != nil {
				if err.Error() != test.expectedErr.Error() {
					t.Fatalf("expected error: %v, got: %v", test.expectedErr, err)
				}
			}

			if cidr != test.expectedCIDR {
				t.Errorf("expected CIDR %q, got %q", test.expectedCIDR, cidr)
			}
		})
	}
}

func Test_preferIPv6(t *testing.T) {
	tests := map[string]struct {
		expected    bool
		cidrs       []string
		expectedErr error
	}{
		"should prefer ipv4 in single stack ipv4 cluster": {
			expected:    false,
			cidrs:       []string{"10.0.0.0/16"},
			expectedErr: nil,
		},
		"should prefer ipv6 in single stack ipv6 cluster": {
			expected:    true,
			cidrs:       []string{"fd00::/64"},
			expectedErr: nil,
		},
		"should prefer ipv4 in v4-primary dual stack cluster": {
			expected:    false,
			cidrs:       []string{"10.0.0.0/16", "fd00::/64"},
			expectedErr: nil,
		},
		"should prefer ipv6 in v6-primary dual stack cluster": {
			expected:    true,
			cidrs:       []string{"fd00::/64", "10.0.0.0/16"},
			expectedErr: nil,
		},
		"should return error on empty cidr list": {
			expected:    false,
			cidrs:       []string{},
			expectedErr: fmt.Errorf("preferIPv6: no serviceCIDRs passed"),
		},
		"should return error on invalid cidr": {
			expected:    false,
			cidrs:       []string{"not a cidr"},
			expectedErr: fmt.Errorf("invalid CIDR address: not a cidr"),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result, err := preferIPv6(test.cidrs)
			if err == nil && test.expectedErr != nil {
				t.Fatalf("didn't get an error, expected: %v", test.expectedErr)
			}

			if err != nil && test.expectedErr == nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if err != nil && test.expectedErr != nil {
				if err.Error() != test.expectedErr.Error() {
					t.Fatalf("expected error: %v, got: %v", test.expectedErr, err)
				}
			}

			if result != test.expected {
				t.Errorf("expected result %v, got %v", test.expected, result)
			}
		})
	}
}
