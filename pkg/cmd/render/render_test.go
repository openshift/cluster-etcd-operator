package render

import (
	"os"
	"reflect"
	"testing"
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
    - cidr: 10.128.0.0/14
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

func TestNetworkConfigIpv4(t *testing.T) {
	renderConfig := TemplateData{}
	if err := discoverCIDRsFromNetwork([]byte(networkConfigIpv4), &renderConfig); err != nil {
		t.Errorf("failed discoverCIDRs: %v", err)
	}
	if !reflect.DeepEqual(renderConfig.ServiceCIDR, expectedServiceIPv4CIDR) {
		t.Errorf("Got: %v, expected: %v", renderConfig.ServiceCIDR, expectedServiceIPv4CIDR)
	}
}

func TestNetworkConfigMixed(t *testing.T) {
	renderConfig := TemplateData{}
	if err := discoverCIDRsFromNetwork([]byte(networkConfigMixed), &renderConfig); err != nil {
		t.Errorf("failed discoverCIDRs: %v", err)
	}
	if !reflect.DeepEqual(renderConfig.ServiceCIDR, expectedServiceMixedCIDR) {
		t.Errorf("Got: %v, expected: %v", renderConfig.ServiceCIDR, expectedServiceMixedCIDR)
	}
}

func TestNetworkConfigMixedSwap(t *testing.T) {
	renderConfig := TemplateData{}
	if err := discoverCIDRsFromNetwork([]byte(networkConfigMixedSwap), &renderConfig); err != nil {
		t.Errorf("failed discoverCIDRs: %v", err)
	}
	if !reflect.DeepEqual(renderConfig.ServiceCIDR, expectedServiceMixedSwapCIDR) {
		t.Errorf("Got: %v, expected: %v", renderConfig.ServiceCIDR, expectedServiceMixedSwapCIDR)
	}
}

func TestNetworkConfigSingleStack(t *testing.T) {
	renderConfig := TemplateData{}
	if err := discoverCIDRsFromNetwork([]byte(networkConfigIPv6SingleStack), &renderConfig); err != nil {
		t.Errorf("failed discoverCIDRs: %v", err)
	}
	if !reflect.DeepEqual(renderConfig.ServiceCIDR, expectedServiceSingleStackCIDR) {
		t.Errorf("Got: %v, expected: %v", renderConfig.ServiceCIDR, expectedServiceSingleStackCIDR)
	}
}

func runRender(args ...string) error {
	c := NewRenderCommand(os.Stderr)
	os.Args = append([]string{""}, args...)
	return c.Execute()
}
