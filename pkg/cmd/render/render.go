package render

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"

	"github.com/ghodss/yaml"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/render/options"
	"github.com/openshift/library-go/pkg/assets"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog"
)

// renderOpts holds values to drive the render command.
type renderOpts struct {
	manifest options.ManifestOptions
	generic  options.GenericOptions

	errOut                   io.Writer
	etcdCAFile               string
	etcdMetricCAFile         string
	etcdDiscoveryDomain      string
	etcdImage                string
	clusterEtcdOperatorImage string
	setupEtcdEnvImage        string
	kubeClientAgentImage     string
	clusterConfigFile        string
	clusterNetworkFile       string
	bootstrapIP              string
}

// NewRenderCommand creates a render command.
func NewRenderCommand(errOut io.Writer) *cobra.Command {
	renderOpts := renderOpts{
		generic:  *options.NewGenericOptions(),
		manifest: *options.NewManifestOptions("etcd"),
		errOut:   errOut,
	}
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render etcd bootstrap manifests, secrets and configMaps",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(renderOpts.errOut, err.Error())
				}
			}

			must(renderOpts.Validate)
			must(renderOpts.Complete)
			must(renderOpts.Run)
		},
	}

	renderOpts.AddFlags(cmd.Flags())

	return cmd
}

func (r *renderOpts) AddFlags(fs *pflag.FlagSet) {
	r.manifest.AddFlags(fs, "etcd")
	r.generic.AddFlags(fs)

	fs.StringVar(&r.etcdCAFile, "etcd-ca", r.etcdCAFile, "path to etcd CA certificate")
	fs.StringVar(&r.etcdMetricCAFile, "etcd-metric-ca", r.etcdMetricCAFile, "path to etcd metric CA certificate")
	fs.StringVar(&r.etcdImage, "manifest-etcd-image", r.etcdImage, "etcd manifest image")
	fs.StringVar(&r.clusterEtcdOperatorImage, "manifest-cluster-etcd-operator-image", r.clusterEtcdOperatorImage, "cluster-etcd-operator manifest image")
	fs.StringVar(&r.kubeClientAgentImage, "manifest-kube-client-agent-image", r.kubeClientAgentImage, "kube-client-agent manifest image")
	fs.StringVar(&r.setupEtcdEnvImage, "manifest-setup-etcd-env-image", r.setupEtcdEnvImage, "setup-etcd-env manifest image")
	fs.StringVar(&r.etcdDiscoveryDomain, "etcd-discovery-domain", r.etcdDiscoveryDomain, "etcd discovery domain")
	fs.StringVar(&r.clusterConfigFile, "cluster-config-file", r.clusterConfigFile, "Openshift Cluster API Config file.")
	fs.StringVar(&r.clusterNetworkFile, "cluster-network-file", r.clusterNetworkFile, "Openshift Cluster Network API Config file.")
	fs.StringVar(&r.bootstrapIP, "bootstrap-ip", r.bootstrapIP, "bootstrap IP used to indicate where to find the first etcd endpoint")
}

// Validate verifies the inputs.
func (r *renderOpts) Validate() error {
	if err := r.manifest.Validate(); err != nil {
		return err
	}
	if err := r.generic.Validate(); err != nil {
		return err
	}
	if len(r.etcdCAFile) == 0 {
		return errors.New("missing required flag: --etcd-ca")
	}
	if len(r.etcdMetricCAFile) == 0 {
		return errors.New("missing required flag: --etcd-metric-ca")
	}
	if len(r.etcdImage) == 0 {
		return errors.New("missing required flag: --manifest-etcd-image")
	}
	if len(r.kubeClientAgentImage) == 0 {
		return errors.New("missing required flag: --manifest-kube-client-agent-image")
	}
	if len(r.setupEtcdEnvImage) == 0 {
		return errors.New("missing required flag: --manifest-setup-etcd-env-image")
	}
	if len(r.clusterEtcdOperatorImage) == 0 {
		return errors.New("missing required flag: --manifest-cluster-etcd-operator-image")
	}
	if len(r.etcdDiscoveryDomain) == 0 {
		return errors.New("missing required flag: --etcd-discovery-domain")
	}
	if len(r.clusterConfigFile) == 0 {
		return errors.New("missing required flag: --cluster-config-file")
	}
	return nil
}

// Complete fills in missing values before command execution.
func (r *renderOpts) Complete() error {
	if err := r.manifest.Complete(); err != nil {
		return err
	}
	if err := r.generic.Complete(); err != nil {
		return err
	}
	return nil
}

type TemplateData struct {
	options.ManifestConfig
	options.FileConfig

	// EtcdDiscoveryDomain is the domain used for SRV discovery.
	EtcdDiscoveryDomain    string
	EtcdServerCertDNSNames string
	EtcdPeerCertDNSNames   string

	// ClusterCIDR is the IP range for pod IPs.
	ClusterCIDR []string

	// ServiceClusterIPRange is the IP range for service IPs.
	ServiceCIDR []string

	// SingleStackIPv6 is true if the stack is IPv6 only.
	SingleStackIPv6 bool

	// Hostname as reported by the kernel
	Hostname string

	// BootstrapIP is address of the bootstrap node
	BootstrapIP string
}

type StaticFile struct {
	name           string
	source         string
	destinationDir string
	mode           os.FileMode
	Data           []byte
}

func newTemplateData(opts *renderOpts) (*TemplateData, error) {
	templateData := TemplateData{
		ManifestConfig: options.ManifestConfig{
			Images: options.Images{
				Etcd:            opts.etcdImage,
				SetupEtcdEnv:    opts.setupEtcdEnvImage,
				KubeClientAgent: opts.kubeClientAgentImage,
			},
		},
		EtcdDiscoveryDomain: opts.etcdDiscoveryDomain,
		EtcdServerCertDNSNames: strings.Join([]string{
			"localhost",
			"etcd.kube-system.svc",
			"etcd.kube-system.svc.cluster.local",
			"etcd.openshift-etcd.svc",
			"etcd.openshift-etcd.svc.cluster.local",
		}, ","),
		EtcdPeerCertDNSNames: strings.Join([]string{
			opts.etcdDiscoveryDomain,
		}, ","),
		BootstrapIP: opts.bootstrapIP,
	}

	// remove after flag changes are merged to installer
	var networkConfigFile string
	if opts.clusterNetworkFile == "" {
		networkConfigFile = opts.clusterConfigFile
	} else {
		networkConfigFile = opts.clusterNetworkFile
	}

	if err := templateData.setClusterCIDR(networkConfigFile); err != nil {
		return nil, err
	}
	if err := templateData.setServiceCIDR(networkConfigFile); err != nil {
		return nil, err
	}
	if err := templateData.setSingleStackIPv6(); err != nil {
		return nil, err
	}
	if err := templateData.setHostname(); err != nil {
		return nil, err
	}

	// TODO installer should feed value by flag this should be removed
	// derive bootstrapIP from local interfaces if not passed by flag
	if templateData.BootstrapIP == "" {
		if err := templateData.setBootstrapIP(); err != nil {
			return nil, err
		}
	}

	templateData.setEtcdAddress()

	return &templateData, nil
}

// Run contains the logic of the render command.
func (r *renderOpts) Run() error {
	templateData, err := newTemplateData(r)
	if err != nil {
		return err
	}

	if err := r.manifest.ApplyTo(&templateData.ManifestConfig); err != nil {
		return err
	}
	if err := r.generic.ApplyTo(
		&templateData.FileConfig,
		options.Template{FileName: "defaultconfig.yaml", Content: etcd_assets.MustAsset(filepath.Join("etcd", "defaultconfig.yaml"))},
		mustReadTemplateFile(filepath.Join(r.generic.TemplatesDir, "config", "bootstrap-config-overrides.yaml")),
		mustReadTemplateFile(filepath.Join(r.generic.TemplatesDir, "config", "config-overrides.yaml")),
		&templateData,
		nil,
	); err != nil {
		return err
	}

	return WriteFiles(&r.generic, &templateData.FileConfig, templateData)
}

// setBootstrapIP gets a list of IPs from local interfaces and returns the first IPv4 or IPv6 address.
func (t *TemplateData) setBootstrapIP() error {
	var bootstrapIP string
	ips, err := ipAddrs()
	if err != nil {
		return err
	}

	for _, addr := range ips {
		ip := net.ParseIP(addr)
		// IPv6
		if t.SingleStackIPv6 && ip.To4() == nil {
			bootstrapIP = addr
			break
		}
		// IPv4
		if !t.SingleStackIPv6 && ip.To4() != nil {
			bootstrapIP = addr
			break
		}
	}

	if bootstrapIP == "" {
		return fmt.Errorf("no IP address found for bootstrap node")
	}

	t.BootstrapIP = bootstrapIP
	return nil
}

func (t *TemplateData) setEtcdAddress() {
	// IPv4
	allAddresses := "0.0.0.0"
	localhost := "127.0.0.1"
	bootstrapIP := t.BootstrapIP

	// IPv6
	if t.SingleStackIPv6 {
		allAddresses = "::"
		localhost = "[::1]"
		bootstrapIP = "[" + t.BootstrapIP + "]"
	}

	etcdAddress := options.EtcdAddress{
		ListenClient:       net.JoinHostPort(allAddresses, "2379"),
		ListenPeer:         net.JoinHostPort(allAddresses, "2380"),
		LocalHost:          localhost,
		ListenMetricServer: net.JoinHostPort(allAddresses, "9978"),
		ListenMetricProxy:  net.JoinHostPort(allAddresses, "9979"),
		EscapedBootstrapIP: bootstrapIP,
	}

	t.ManifestConfig.EtcdAddress = etcdAddress
}

func (t *TemplateData) getClusterConfigFromFile(clusterConfigFile string) (*unstructured.Unstructured, error) {
	clusterConfigFileData, err := ioutil.ReadFile(clusterConfigFile)
	if err != nil {
		return nil, err
	}
	configJson, err := yaml.YAMLToJSON(clusterConfigFileData)
	if err != nil {
		return nil, err
	}
	clusterConfigObj, err := runtime.Decode(unstructured.UnstructuredJSONScheme, configJson)
	if err != nil {
		return nil, err
	}
	clusterConfig, ok := clusterConfigObj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("unexpected object in %t", clusterConfigObj)
	}
	return clusterConfig, nil
}

func (t *TemplateData) setServiceCIDR(clusterNetworkFile string) error {
	clusterConfig, err := t.getClusterConfigFromFile(clusterNetworkFile)
	if err != nil {
		return err
	}
	serviceCIDR, found, err := unstructured.NestedStringSlice(
		clusterConfig.Object, "spec", "serviceNetwork")
	if found && err == nil {
		t.ServiceCIDR = serviceCIDR
	}
	if err != nil {
		return err
	}
	return nil
}

func (t *TemplateData) setHostname() error {
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	t.Hostname = hostname
	return nil
}

func (t *TemplateData) setClusterCIDR(clusterNetworkFile string) error {
	clusterConfig, err := t.getClusterConfigFromFile(clusterNetworkFile)
	if err != nil {
		return err
	}
	clusterCIDR, found, err := unstructured.NestedSlice(
		clusterConfig.Object, "spec", "clusterNetwork")
	if found && err == nil {
		for key := range clusterCIDR {
			slice, ok := clusterCIDR[key].(map[string]interface{})
			if !ok {
				return fmt.Errorf("unexpected object in %t", clusterCIDR[key])
			}
			if CIDR, found, err := unstructured.NestedString(slice, "cidr"); found && err == nil {
				t.ClusterCIDR = append(t.ClusterCIDR, CIDR)
			}
		}
	}
	if err != nil {
		return err
	}
	return nil
}

func (t *TemplateData) setSingleStackIPv6() error {
	singleStack, err := isSingleStackIPv6(t.ServiceCIDR)
	if err != nil {
		return err
	}
	if singleStack {
		t.SingleStackIPv6 = true
	}
	return nil
}

// TODO: add to util
func EscapeIpv6Address(addr string) (string, error) {
	if ip := net.ParseIP(addr); ip == nil {
		return "", fmt.Errorf("invalid ipaddress: %s", addr)
	}
	ip := net.ParseIP(addr)
	switch {
	case ip == nil:
		return "", fmt.Errorf("invalid ipaddress: %s", addr)
	case ip.To4() != nil:
		return "", fmt.Errorf("address must be IPv6: %s", addr)
	}
	return fmt.Sprintf("[%s]", addr), nil
}

// WriteFiles writes the manifests and the bootstrap config file.
func WriteFiles(opt *options.GenericOptions, fileConfig *options.FileConfig, templateData interface{}, additionalPredicates ...assets.FileInfoPredicate) error {
	// write assets
	for _, manifestDir := range []string{"bootstrap-manifests", "manifests"} {
		manifests, err := assets.New(filepath.Join(opt.TemplatesDir, manifestDir), templateData, append(additionalPredicates, assets.OnlyYaml)...)
		if err != nil {
			return fmt.Errorf("failed rendering assets: %v", err)
		}
		if err := manifests.WriteFiles(filepath.Join(opt.AssetOutputDir, manifestDir)); err != nil {
			return fmt.Errorf("failed writing assets to %q: %v", filepath.Join(opt.AssetOutputDir, manifestDir), err)
		}
	}

	// create bootstrap configuration
	if err := ioutil.WriteFile(opt.ConfigOutputFile, fileConfig.BootstrapConfig, 0644); err != nil {
		return fmt.Errorf("failed to write merged config to %q: %v", opt.ConfigOutputFile, err)
	}

	return nil
}

func isSingleStackIPv6(serviceCIDRs []string) (bool, error) {
	if len(serviceCIDRs) == 0 {
		return false, fmt.Errorf("isSingleStackIPv6: no serviceCIDRs passed")
	}
	for _, cidr := range serviceCIDRs {
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil {
			return false, err
		}
		if ip.To4() != nil {
			return false, nil
		}
	}
	return true, nil
}

func mustReadTemplateFile(fname string) options.Template {
	bs, err := ioutil.ReadFile(fname)
	if err != nil {
		panic(fmt.Sprintf("Failed to load %q: %v", fname, err))
	}
	return options.Template{FileName: fname, Content: bs}
}

func ipAddrs() ([]string, error) {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips, err
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		if !ip.IsGlobalUnicast() {
			continue // we only want global unicast address
		}
		ips = append(ips, ip.String())
	}
	return ips, nil
}
