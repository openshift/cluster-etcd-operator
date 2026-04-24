package render

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ghodss/yaml"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"k8s.io/apimachinery/pkg/util/sets"
)

var (
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
    featureGates: [ShortCertRotation=false]
`
	clusterConfigMapSingleNodeBootstrapInPlace = `
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
    featureGates: [ShortCertRotation=false]
`

	clusterConfigMapSingleNode = `
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
    featureGates: [ShortCertRotation=false]
`
	clusterConfigMapTwoNode = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-config-v1
  namespace: kube-system
data:
  install-config: |
    apiVersion: v1
    baseDomain: devcluster.openshift.com
    controlPlane:
      architecture: amd64
      hyperthreading: Enabled
      name: master
      replicas: 2
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
      none: {}
    publish: External
    featureGates: [ShortCertRotation=false]
`

	clusterConfigMapTwoNodeWithArbiter = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-config-v1
  namespace: kube-system
data:
  install-config: |
    apiVersion: v1
    baseDomain: devcluster.openshift.com
    controlPlane:
      architecture: amd64
      hyperthreading: Enabled
      name: master
      replicas: 2
    arbiter:
      architecture: amd64
      hyperthreading: Enabled
      name: arbiter
      replicas: 1
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
      none: {}
    publish: External
    featureGates: [ShortCertRotation=false]
`
)

type testConfig struct {
	clusterNetworkConfig                    string
	infraConfig                             string
	clusterConfigMap                        string
	delayedHABootstrapScalingStrategyMarker string
}

func TestMain(m *testing.M) {
	// TODO: implement tests for bootstrap IP determination
	defaultBootstrapIPLocator = &fakeBootstrapIPLocator{ip: net.ParseIP("10.0.0.1")}
	os.Exit(m.Run())
}

func TestRenderIpv4(t *testing.T) {
	config := &testConfig{
		clusterNetworkConfig: networkConfigIpv4,
		infraConfig:          infraConfig,
		clusterConfigMap:     clusterConfigMap,
	}

	testRender(t, config)
}

func testRender(t *testing.T, tc *testConfig) {
	var errOut io.Writer
	dir := t.TempDir()

	clusterConfigPath := filepath.Join(dir, "cluster-network-02-config.yaml")
	if err := os.WriteFile(clusterConfigPath, []byte(tc.clusterNetworkConfig), 0600); err != nil {
		t.Fatal(err)
	}

	infraConfigPath := filepath.Join(dir, "cluster-infrastructure-02-config.yaml")
	if err := os.WriteFile(infraConfigPath, []byte(tc.infraConfig), 0600); err != nil {
		t.Fatal(err)
	}

	clusterConfigMapPath := filepath.Join(dir, "cluster-config.yaml")
	if err := os.WriteFile(clusterConfigMapPath, []byte(tc.clusterConfigMap), 0600); err != nil {
		t.Fatal(err)
	}

	render := renderOpts{
		assetOutputDir:       dir,
		templateDir:          filepath.Join("../../..", "bindata", "bootkube"),
		errOut:               errOut,
		networkConfigFile:    clusterConfigPath,
		infraConfigFile:      infraConfigPath,
		clusterConfigMapFile: clusterConfigMapPath,
	}

	if err := render.Run(); err != nil {
		t.Errorf("failed render.Run(): %v", err)
	}
}

func TestTemplateDataIpv4(t *testing.T) {
	config := &testConfig{
		clusterNetworkConfig: networkConfigIpv4,
		infraConfig:          infraConfig,
		clusterConfigMap:     clusterConfigMap,
	}
	testTemplateData(
		t,
		config,
		hasEtcdAddress("127.0.0.1"),
		hasClusterCIDR("10.128.0.0/14"),
		hasServiceCIDR("172.30.0.0/16"),
		prefersIPv6(false),
	)
}

func TestRenderScalingStrategy(t *testing.T) {
	tests := []struct {
		name                 string
		clusterConfigMap     string
		delayedHAMarkerFile  string
		expectedStrategy     ceohelpers.BootstrapScalingStrategy
		additionalValidators []func(*testing.T, *TemplateData)
	}{
		{
			name:             "BootstrapInPlace",
			clusterConfigMap: clusterConfigMapSingleNodeBootstrapInPlace,
			expectedStrategy: ceohelpers.BootstrapInPlaceStrategy,
			additionalValidators: []func(*testing.T, *TemplateData){
				hasEtcdEndpointConfigmapData("MTAuMC4wLjE: 10.0.0.1"),
			},
		},
		{
			name:             "Unsafe",
			clusterConfigMap: clusterConfigMapSingleNode,
			expectedStrategy: ceohelpers.UnsafeScalingStrategy,
		},
		{
			name:                "DelayedHA",
			clusterConfigMap:    clusterConfigMap,
			delayedHAMarkerFile: "/dev/null", // exists
			expectedStrategy:    ceohelpers.DelayedHAScalingStrategy,
			additionalValidators: []func(*testing.T, *TemplateData){
				hasNamespaceAnnotations(map[string]string{ceohelpers.DelayedBootstrapScalingStrategyAnnotation: ""}),
			},
		},
		{
			name:             "TwoNode",
			clusterConfigMap: clusterConfigMapTwoNode,
			expectedStrategy: ceohelpers.TwoNodeScalingStrategy,
		},
		{
			name:             "TwoNodeWithArbiter",
			clusterConfigMap: clusterConfigMapTwoNodeWithArbiter,
			expectedStrategy: ceohelpers.HAScalingStrategy,
		},
		{
			name:                "DelayedTwoNode",
			clusterConfigMap:    clusterConfigMapTwoNode,
			delayedHAMarkerFile: "/dev/null", // exists
			expectedStrategy:    ceohelpers.DelayedTwoNodeScalingStrategy,
			additionalValidators: []func(*testing.T, *TemplateData){
				hasNamespaceAnnotations(map[string]string{ceohelpers.DelayedBootstrapScalingStrategyAnnotation: ""}),
			},
		},
		{
			name:                "DelayedTwoNodeWithArbiter",
			clusterConfigMap:    clusterConfigMapTwoNodeWithArbiter,
			delayedHAMarkerFile: "/dev/null", // exists
			expectedStrategy:    ceohelpers.DelayedHAScalingStrategy,
			additionalValidators: []func(*testing.T, *TemplateData){
				hasNamespaceAnnotations(map[string]string{ceohelpers.DelayedBootstrapScalingStrategyAnnotation: ""}),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &testConfig{
				clusterNetworkConfig:                    networkConfigIpv4,
				infraConfig:                             infraConfig,
				clusterConfigMap:                        tt.clusterConfigMap,
				delayedHABootstrapScalingStrategyMarker: tt.delayedHAMarkerFile,
			}

			validators := append(tt.additionalValidators, hasBootstrapScalingStrategy(tt.expectedStrategy))

			testTemplateData(t, config, validators...)
		})
	}
}

func TestTemplateDataMixed(t *testing.T) {
	config := &testConfig{
		clusterNetworkConfig: networkConfigMixedSwap,
		infraConfig:          infraConfig,
		clusterConfigMap:     clusterConfigMap,
	}
	testTemplateData(
		t,
		config,
		hasEtcdAddress("[::1]"),
		hasClusterCIDR("10.128.10.0/14"),
		hasServiceCIDR("2001:db8::/32", "172.30.0.0/16"),
		prefersIPv6(true),
	)
}

func TestTemplateDataSingleStack(t *testing.T) {
	config := &testConfig{
		clusterNetworkConfig: networkConfigIPv6SingleStack,
		infraConfig:          infraConfig,
		clusterConfigMap:     clusterConfigMap,
	}
	testTemplateData(
		t,
		config,
		hasEtcdAddress("[::1]"),
		hasClusterCIDR("10.128.0.0/14"),
		hasServiceCIDR("2001:db8::/32"),
		prefersIPv6(true),
	)
}

func hasClusterCIDR(expected ...string) func(*testing.T, *TemplateData) {
	return func(t *testing.T, td *TemplateData) {
		if len(td.ClusterCIDR) != len(expected) {
			t.Errorf("len(ClusterCIDR) want: %d got: %d", len(expected), len(td.ClusterCIDR))
			return
		}
		for i, cidr := range expected {
			if td.ClusterCIDR[i] != cidr {
				t.Errorf("ClusterCIDR[%d] want: %q got: %q", i, cidr, td.ClusterCIDR[i])
			}
		}
	}
}

func hasServiceCIDR(expected ...string) func(*testing.T, *TemplateData) {
	return func(t *testing.T, td *TemplateData) {
		if len(td.ServiceCIDR) != len(expected) {
			t.Errorf("len(ServiceCIDR) want: %d got: %d", len(expected), len(td.ServiceCIDR))
			return
		}
		for i, cidr := range expected {
			if td.ServiceCIDR[i] != cidr {
				t.Errorf("ServiceCIDR[%d] want: %q got: %q", i, cidr, td.ServiceCIDR[i])
			}
		}
	}
}

func prefersIPv6(expected bool) func(*testing.T, *TemplateData) {
	return func(t *testing.T, td *TemplateData) {
		if td.PreferIPv6 != expected {
			t.Errorf("PreferIPv6 want: %v got: %v", expected, td.PreferIPv6)
		}
	}
}

func hasEtcdAddress(localhost string) func(*testing.T, *TemplateData) {
	return func(t *testing.T, td *TemplateData) {
		if td.EtcdAddress.LocalHost != localhost {
			t.Errorf("LocalHost want: %q got: %q", localhost, td.EtcdAddress.LocalHost)
		}
	}
}

func hasBootstrapScalingStrategy(expected ceohelpers.BootstrapScalingStrategy) func(*testing.T, *TemplateData) {
	return func(t *testing.T, td *TemplateData) {
		if td.BootstrapScalingStrategy != expected {
			t.Errorf("BootstrapScalingStrategy want: %q got: %q", expected, td.BootstrapScalingStrategy)
		}
	}
}

func hasEtcdEndpointConfigmapData(expected string) func(*testing.T, *TemplateData) {
	return func(t *testing.T, td *TemplateData) {
		if td.EtcdEndpointConfigmapData != expected {
			t.Errorf("EtcdEndpointConfigmapData want: %q got: %q", expected, td.EtcdEndpointConfigmapData)
		}
	}
}

func hasNamespaceAnnotations(expected map[string]string) func(*testing.T, *TemplateData) {
	return func(t *testing.T, td *TemplateData) {
		if !reflect.DeepEqual(td.NamespaceAnnotations, expected) {
			t.Errorf("NamespaceAnnotations want: %q got: %q", expected, td.NamespaceAnnotations)
		}
	}
}

func testTemplateData(t *testing.T, tc *testConfig, validators ...func(*testing.T, *TemplateData)) {
	var errOut io.Writer
	dir := t.TempDir()

	clusterConfigPath := filepath.Join(dir, "cluster-network-02-config.yaml")
	if err := os.WriteFile(clusterConfigPath, []byte(tc.clusterNetworkConfig), 0600); err != nil {
		t.Fatal(err)
	}

	infraConfigPath := filepath.Join(dir, "cluster-infrastructures-02-config.yaml")
	if err := os.WriteFile(infraConfigPath, []byte(tc.infraConfig), 0600); err != nil {
		t.Fatal(err)
	}

	clusterConfigMapPath := filepath.Join(dir, "cluster-config.yaml")
	if err := os.WriteFile(clusterConfigMapPath, []byte(tc.clusterConfigMap), 0600); err != nil {
		t.Fatal(err)
	}

	render := &renderOpts{
		assetOutputDir:                          dir,
		templateDir:                             filepath.Join("../../..", "bindata", "bootkube"),
		errOut:                                  errOut,
		networkConfigFile:                       clusterConfigPath,
		infraConfigFile:                         infraConfigPath,
		clusterConfigMapFile:                    clusterConfigMapPath,
		delayedHABootstrapScalingStrategyMarker: tc.delayedHABootstrapScalingStrategyMarker,
	}

	got, err := newTemplateData(render)
	if err != nil {
		t.Fatal(err)
	}

	for _, validate := range validators {
		validate(t, got)
	}
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
			var installConfig map[string]any
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

func Test_getFeatureGates(t *testing.T) {
	tests := map[string]struct {
		installConfig    string
		expectedEnabled  sets.Set[configv1.FeatureGateName]
		expectedDisabled sets.Set[configv1.FeatureGateName]
	}{
		"no feature gates defined": {
			installConfig: `
apiVersion: v1
metadata:
  name: my-cluster
`,
			expectedEnabled:  sets.New[configv1.FeatureGateName](),
			expectedDisabled: sets.New(features.FeatureShortCertRotation),
		},
		"enabled feature gates defined": {
			installConfig: `
apiVersion: v1
metadata:
  name: my-cluster
featureGates: [ShortCertRotation=true]
`,
			expectedEnabled:  sets.New(features.FeatureShortCertRotation),
			expectedDisabled: sets.New[configv1.FeatureGateName](),
		},
		"disabled feature gates defined": {
			installConfig: `
apiVersion: v1
metadata:
  name: my-cluster
featureGates: [ShortCertRotation=false]
`,
			expectedEnabled:  sets.New[configv1.FeatureGateName](),
			expectedDisabled: sets.New(features.FeatureShortCertRotation),
		},
		"mixed feature gates defined": {
			installConfig: `
apiVersion: v1
metadata:
  name: my-cluster
featureGates: [ShortCertRotation=true, UpgradeStatus=false]
`,
			expectedEnabled:  sets.New(features.FeatureShortCertRotation),
			expectedDisabled: sets.New(features.FeatureGateUpgradeStatus),
		},
		"unexpected data": {
			installConfig: `
apiVersion: v1
metadata:
  name: my-cluster
featureGates: [ShortCertRotation=true, UpgradeStatus=foobar]
`,
			expectedEnabled:  sets.New(features.FeatureShortCertRotation),
			expectedDisabled: sets.New[configv1.FeatureGateName](),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var installConfig map[string]any
			if err := yaml.Unmarshal([]byte(test.installConfig), &installConfig); err != nil {
				panic(err)
			}
			actualEnabled, actualDisabled := getFeatureGatesStatus(installConfig)
			assert.Equal(t, actualEnabled, test.expectedEnabled)
			assert.Equal(t, actualDisabled, test.expectedDisabled)
		})
	}
}
