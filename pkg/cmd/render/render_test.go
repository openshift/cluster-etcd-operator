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

type testConfig struct {
	t                 *testing.T
	networkConfigFile string
	clusterConfigFile string
	want              TemplateData
	bootstrapIP       string
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
		BootstrapIP:     "10.128.0.12",
	}

	config := &testConfig{
		t:                 t,
		clusterConfigFile: "cluster-config-aws.yaml",
		networkConfigFile: "network-config.yaml",
		want:              want,
		bootstrapIP:       "10.128.0.12",
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

	generic := options.GenericOptions{
		AssetInputDir:    dir,
		AssetOutputDir:   dir,
		TemplatesDir:     filepath.Join("../../..", "bindata", "bootkube"),
		ConfigOutputFile: filepath.Join(dir, "config"),
	}

	render := renderOpts{
		generic:            generic,
		manifest:           *options.NewManifestOptions("etcd"),
		errOut:             errOut,
		clusterConfigFile:  filepath.Join("testdata", tc.clusterConfigFile),
		clusterNetworkFile: filepath.Join("testdata", tc.networkConfigFile),
		bootstrapIP:        tc.bootstrapIP,
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
		BootstrapIP:     "10.128.0.12",
	}

	config := &testConfig{
		t:                 t,
		clusterConfigFile: "cluster-config-aws.yaml",
		networkConfigFile: "network-config.yaml",
		want:              want,
		bootstrapIP:       "10.128.0.12",
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
		ClusterCIDR:     []string{"fd01::/48"},
		ServiceCIDR:     []string{"fd02::/112"},
		SingleStackIPv6: true,
		BootstrapIP:     "fe80::d66c:724c:13d4:829c",
	}

	config := &testConfig{
		t:                 t,
		clusterConfigFile: "singlestack/cluster-config-azure.yaml",
		networkConfigFile: "singlestack/network-config.yaml",
		want:              want,
		bootstrapIP:       "fe80::d66c:724c:13d4:829c",
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

	generic := options.GenericOptions{
		AssetInputDir:    dir,
		AssetOutputDir:   dir,
		TemplatesDir:     filepath.Join("../../..", "bindata", "bootkube"),
		ConfigOutputFile: filepath.Join(dir, "config"),
	}

	render := &renderOpts{
		generic:            generic,
		manifest:           *options.NewManifestOptions("etcd"),
		errOut:             errOut,
		clusterConfigFile:  filepath.Join("testdata", tc.clusterConfigFile),
		clusterNetworkFile: filepath.Join("testdata", tc.networkConfigFile),
		bootstrapIP:        tc.bootstrapIP,
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
