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
	etcdStaticResourcesDir   string
	etcdConfigDir            string
	setupEtcdEnvImage        string
	kubeClientAgentImage     string
	clusterConfigFile        string
}

// NewRenderCommand creates a render command.
func NewRenderCommand(errOut io.Writer) *cobra.Command {
	renderOpts := renderOpts{
		generic:                *options.NewGenericOptions(),
		manifest:               *options.NewManifestOptions("etcd"),
		errOut:                 errOut,
		etcdStaticResourcesDir: "/etc/kubernetes/static-pod-resources/etcd-member",
		etcdConfigDir:          "/etc/etcd",
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
	fs.StringVar(&r.etcdStaticResourcesDir, "etcd-static-resources-dir", r.etcdStaticResourcesDir, "path to etcd static resources directory")
	fs.StringVar(&r.etcdConfigDir, "etcd-config-dir", r.etcdConfigDir, "path to etcd config directory")
	fs.StringVar(&r.clusterConfigFile, "cluster-config-file", r.clusterConfigFile, "Openshift Cluster API Config file.")
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
}

type StaticFile struct {
	name           string
	source         string
	destinationDir string
	mode           os.FileMode
	Data           []byte
}

// Run contains the logic of the render command.
func (r *renderOpts) Run() error {
	renderConfig := TemplateData{
		ManifestConfig: options.ManifestConfig{
			Images: options.Images{
				Etcd:            r.etcdImage,
				SetupEtcdEnv:    r.setupEtcdEnvImage,
				KubeClientAgent: r.kubeClientAgentImage,
			},
		},
		EtcdDiscoveryDomain: r.etcdDiscoveryDomain,
		EtcdServerCertDNSNames: strings.Join([]string{
			"localhost",
			"etcd.kube-system.svc",
			"etcd.kube-system.svc.cluster.local",
			"etcd.openshift-etcd.svc",
			"etcd.openshift-etcd.svc.cluster.local",
		}, ","),
		EtcdPeerCertDNSNames: strings.Join([]string{
			r.etcdDiscoveryDomain,
		}, ","),
	}
	if len(r.clusterConfigFile) > 0 {
		clusterConfigFileData, err := ioutil.ReadFile(r.clusterConfigFile)
		if err != nil {
			return err
		}
		if err = discoverCIDRs(clusterConfigFileData, &renderConfig); err != nil {
			return fmt.Errorf("unable to parse restricted CIDRs from config %q: %v", r.clusterConfigFile, err)
		}
		if renderConfig.ServiceCIDR == nil {
			return fmt.Errorf("sdn serviceCIDR not found")
		}
		singleStack, err := isSingleStackIPv6(renderConfig.ServiceCIDR)
		if err != nil {
			return nil
		}
		// TODO make me pretty and a function for ez testing
		if singleStack {
			// IPv6
			etcdAddress := options.EtcdAddress{
				ListenClient:       net.JoinHostPort(net.IPv6zero.String(), "2379"),
				ListenPeer:         net.JoinHostPort(net.IPv6zero.String(), "2380"),
				LocalHost:          "[::]",
				ListenMetricServer: net.JoinHostPort(net.IPv6zero.String(), "9978"),
				ListenMetricProxy:  net.JoinHostPort(net.IPv6zero.String(), "9979"),
			}
			renderConfig.ManifestConfig.EtcdAddress = etcdAddress
		} else {
			// IPv4
			etcdAddress := options.EtcdAddress{
				ListenClient:       net.JoinHostPort(net.IPv4zero.String(), "2379"),
				ListenPeer:         net.JoinHostPort(net.IPv4zero.String(), "2380"),
				LocalHost:          "127.0.0.1",
				ListenMetricServer: net.JoinHostPort(net.IPv4zero.String(), "9978"),
				ListenMetricProxy:  net.JoinHostPort(net.IPv4zero.String(), "9979"),
			}
			renderConfig.ManifestConfig.EtcdAddress = etcdAddress
		}
	}

	if err := r.manifest.ApplyTo(&renderConfig.ManifestConfig); err != nil {
		return err
	}
	if err := r.generic.ApplyTo(
		&renderConfig.FileConfig,
		options.Template{FileName: "defaultconfig.yaml", Content: etcd_assets.MustAsset(filepath.Join("etcd", "defaultconfig.yaml"))},
		mustReadTemplateFile(filepath.Join(r.generic.TemplatesDir, "config", "bootstrap-config-overrides.yaml")),
		mustReadTemplateFile(filepath.Join(r.generic.TemplatesDir, "config", "config-overrides.yaml")),
		&renderConfig,
		nil,
	); err != nil {
		return err
	}

	return WriteFiles(&r.generic, &renderConfig.FileConfig, renderConfig)
}

// parseStaticTemplateFile takes a path to a yaml template file returns a populated File struct.
func parseStaticTemplateFile(path string) (*File, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	file := new(File)
	if err := yaml.Unmarshal(data, file); err != nil {
		return nil, fmt.Errorf("failed to unmarshal file into struct: %v", err)
	}
	return file, nil
}

// populateFileData takes a slice of StaticFile and populates Data from StaticFile.source.
// If source is not populated, skip.
func populateFileData(files []StaticFile) ([]StaticFile, error) {
	for i, file := range files {
		if file.source == "" {
			continue
		}
		data, err := ioutil.ReadFile(file.source)
		if err != nil {
			return nil, err
		}
		files[i].Data = data
	}
	return files, nil
}

func writeStaticFiles(files []StaticFile) error {
	for _, m := range files {
		if len(m.Data) == 0 {
			return fmt.Errorf("file %s has no data:", m.name)
		}
		b := m.Data
		path := filepath.Join(m.destinationDir, m.name)
		dirname := filepath.Dir(path)
		if err := os.MkdirAll(dirname, 0755); err != nil {
			return err
		}
		if err := ioutil.WriteFile(path, b, m.mode); err != nil {
			return err
		}
	}
	return nil
}

func mustReadTemplateFile(fname string) options.Template {
	bs, err := ioutil.ReadFile(fname)
	if err != nil {
		panic(fmt.Sprintf("Failed to load %q: %v", fname, err))
	}
	return options.Template{FileName: fname, Content: bs}
}

func discoverCIDRs(clusterConfigFileData []byte, renderConfig *TemplateData) error {
	if err := discoverCIDRsFromNetwork(clusterConfigFileData, renderConfig); err != nil {
		if err = discoverCIDRsFromClusterAPI(clusterConfigFileData, renderConfig); err != nil {
			return err
		}
	}
	return nil
}

func discoverCIDRsFromNetwork(clusterConfigFileData []byte, renderConfig *TemplateData) error {
	configJson, err := yaml.YAMLToJSON(clusterConfigFileData)
	if err != nil {
		return err
	}
	clusterConfigObj, err := runtime.Decode(unstructured.UnstructuredJSONScheme, configJson)
	if err != nil {
		return err
	}
	clusterConfig, ok := clusterConfigObj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected object in %t", clusterConfigObj)
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
				renderConfig.ClusterCIDR = append(renderConfig.ClusterCIDR, CIDR)
			}
		}
	}
	if err != nil {
		return err
	}
	serviceCIDR, found, err := unstructured.NestedStringSlice(
		clusterConfig.Object, "spec", "serviceNetwork")
	if found && err == nil {
		renderConfig.ServiceCIDR = serviceCIDR
	}
	if err != nil {
		return err
	}
	return nil
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

func discoverCIDRsFromClusterAPI(clusterConfigFileData []byte, renderConfig *TemplateData) error {
	configJson, err := yaml.YAMLToJSON(clusterConfigFileData)
	if err != nil {
		return err
	}
	clusterConfigObj, err := runtime.Decode(unstructured.UnstructuredJSONScheme, configJson)
	if err != nil {
		return err
	}
	clusterConfig, ok := clusterConfigObj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected object in %t", clusterConfigObj)
	}
	clusterCIDR, found, err := unstructured.NestedStringSlice(
		clusterConfig.Object, "spec", "clusterNetwork", "pods", "cidrBlocks")
	if found && err == nil {
		renderConfig.ClusterCIDR = clusterCIDR
	}
	if err != nil {
		return err
	}
	serviceCIDR, found, err := unstructured.NestedStringSlice(
		clusterConfig.Object, "spec", "clusterNetwork", "services", "cidrBlocks")
	if found && err == nil {
		renderConfig.ServiceCIDR = serviceCIDR
	}
	if err != nil {
		return err
	}
	return nil
}

func isSingleStackIPv6(serviceCIDRs []string) (bool, error) {
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
