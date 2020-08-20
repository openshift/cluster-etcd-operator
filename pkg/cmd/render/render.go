package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"

	"github.com/ghodss/yaml"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/render/options"
	"github.com/openshift/library-go/pkg/assets"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
)

// renderOpts holds values to drive the render command.
type renderOpts struct {
	manifest options.ManifestOptions
	generic  options.GenericOptions

	errOut                   io.Writer
	etcdCAFile               string
	etcdCAKeyFile            string
	etcdDiscoveryDomain      string
	etcdImage                string
	clusterEtcdOperatorImage string
	setupEtcdEnvImage        string
	kubeClientAgentImage     string
	networkConfigFile        string
	clusterConfigMapFile     string
	infraConfigFile          string
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

func NewBootstrapIPCommand(errOut io.Writer) *cobra.Command {
	var ipv6 bool
	var clusterConfigFile string
	cmd := &cobra.Command{
		Use:   "show-bootstrap-ip",
		Short: "Discovers and prints the bootstrap node IP",
		Run: func(cmd *cobra.Command, args []string) {
			clusterConfigMap, err := getUnstructured(clusterConfigFile)
			if err != nil {
				fmt.Fprintf(errOut, "%s\n", err)
				os.Exit(1)
			}
			installConfig, err := getInstallConfig(clusterConfigMap)
			if err != nil {
				fmt.Fprintf(errOut, "%s\n", err)
				os.Exit(1)
			}
			cidr, err := getMachineCIDR(installConfig, ipv6)
			if err != nil {
				fmt.Fprintf(errOut, "%s\n", err)
				os.Exit(1)
			}
			excludedIPs, err := getExcludedMachineIPs(installConfig)
			if err != nil {
				fmt.Fprintf(errOut, "%s\n", err)
				os.Exit(1)
			}
			ip, err := defaultBootstrapIPLocator.getBootstrapIP(ipv6, cidr, excludedIPs)
			if err != nil {
				fmt.Fprintf(errOut, "%s\n", err)
				os.Exit(1)
			}
			fmt.Println(ip.String())
		},
	}
	cmd.Flags().BoolVarP(&ipv6, "ipv6", "6", false, "ipv6 mode")
	cmd.Flags().StringVarP(&clusterConfigFile, "cluster-config", "c", "", "cluster config yaml file")
	return cmd
}

func (r *renderOpts) AddFlags(fs *pflag.FlagSet) {
	r.manifest.AddFlags(fs, "etcd")
	r.generic.AddFlags(fs)

	fs.StringVar(&r.etcdCAFile, "etcd-ca", "/assets/tls/etcd-ca-bundle.crt", "path to etcd CA certificate")
	fs.StringVar(&r.etcdCAKeyFile, "etcd-ca-key", "/assets/tls/etcd-signer.key", "path to etcd CA certificate key")
	fs.StringVar(&r.etcdImage, "manifest-etcd-image", r.etcdImage, "etcd manifest image")
	fs.StringVar(&r.clusterEtcdOperatorImage, "manifest-cluster-etcd-operator-image", r.clusterEtcdOperatorImage, "cluster-etcd-operator manifest image")
	fs.StringVar(&r.kubeClientAgentImage, "manifest-kube-client-agent-image", r.kubeClientAgentImage, "kube-client-agent manifest image")
	fs.StringVar(&r.setupEtcdEnvImage, "manifest-setup-etcd-env-image", r.setupEtcdEnvImage, "setup-etcd-env manifest image")
	fs.StringVar(&r.etcdDiscoveryDomain, "etcd-discovery-domain", r.etcdDiscoveryDomain, "etcd discovery domain")
	// TODO: This flag name needs changed to be less confusing.
	fs.StringVar(&r.networkConfigFile, "cluster-config-file", r.networkConfigFile, "File containing the network.config.openshift.io manifest. (Note: the flag name is misleading.)")
	fs.StringVar(&r.clusterConfigMapFile, "cluster-configmap-file", "/assets/manifests/cluster-config.yaml", "File containing the cluster-config-v1 configmap.")
	fs.StringVar(&r.infraConfigFile, "infra-config-file", "/assets/manifests/cluster-infrastructure-02-config.yml", "File containing infrastructure.config.openshift.io manifest.")
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
	if len(r.etcdImage) == 0 {
		return errors.New("missing required flag: --manifest-etcd-image")
	}
	if len(r.clusterEtcdOperatorImage) == 0 {
		return errors.New("missing required flag: --manifest-cluster-etcd-operator-image")
	}
	if len(r.networkConfigFile) == 0 {
		return errors.New("missing required flag: --cluster-config-file")
	}
	if len(r.clusterConfigMapFile) == 0 {
		return errors.New("missing required flag: --cluster-configmap-file")
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

	EtcdServerCertDNSNames string
	EtcdPeerCertDNSNames   string

	// ClusterCIDR is the IP range for pod IPs.
	ClusterCIDR []string

	// ServiceCIDR is the IP range for service IPs.
	ServiceCIDR []string

	// MachineCIDR is the IP range for machine IPs.
	MachineCIDR string

	// SingleStackIPv6 is true if the stack is IPv6 only.
	SingleStackIPv6 bool

	// Hostname as reported by the kernel.
	Hostname string

	// BootstrapIP is address of the bootstrap node.
	BootstrapIP string

	// Platform is the underlying provider the cluster is run on.
	Platform string

	// ComputedEnvVars name/value pairs to populate env: for static pod.
	ComputedEnvVars string
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
				Etcd: opts.etcdImage,
			},
		},
		EtcdServerCertDNSNames: strings.Join([]string{
			"localhost",
			"etcd.kube-system.svc",
			"etcd.kube-system.svc.cluster.local",
			"etcd.openshift-etcd.svc",
			"etcd.openshift-etcd.svc.cluster.local",
		}, ","),
		BootstrapIP: opts.bootstrapIP,
	}

	network, err := getNetwork(opts.networkConfigFile)
	if err != nil {
		return nil, fmt.Errorf("failed to get network config: %w", err)
	}

	infra, err := getInfrastructure(opts.infraConfigFile)
	if err != nil {
		return nil, fmt.Errorf("failed to get infrastructure config: %w", err)
	}

	clusterConfigMap, err := getUnstructured(opts.clusterConfigMapFile)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster configmap: %w", err)
	}
	installConfig, err := getInstallConfig(clusterConfigMap)
	if err != nil {
		return nil, fmt.Errorf("failed to get install config from cluster configmap: %w", err)
	}

	for _, network := range network.Spec.ClusterNetwork {
		templateData.ClusterCIDR = append(templateData.ClusterCIDR, network.CIDR)
	}

	templateData.ServiceCIDR = network.Spec.ServiceNetwork

	if err := templateData.setSingleStackIPv6(templateData.ServiceCIDR); err != nil {
		return nil, err
	}
	if err := templateData.setMachineCIDR(installConfig, templateData.SingleStackIPv6); err != nil {
		return nil, err
	}
	if err := templateData.setHostname(); err != nil {
		return nil, err
	}

	if templateData.BootstrapIP == "" {
		excludedIPs, err := getExcludedMachineIPs(installConfig)
		if err != nil {
			return nil, err
		}
		if err := templateData.setBootstrapIP(templateData.MachineCIDR, templateData.SingleStackIPv6, excludedIPs); err != nil {
			return nil, err
		}
	}

	templateData.setEtcdAddress(templateData.SingleStackIPv6, templateData.BootstrapIP)

	// assume that this is >4.2
	templateData.Platform = string(infra.Status.PlatformStatus.Type)

	if err := templateData.setComputedEnvVars(templateData.Platform); err != nil {
		return nil, err
	}

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

	bootstrapManifestsDir := filepath.Join(r.generic.AssetOutputDir, "bootstrap-manifests")

	caCertData, err := ioutil.ReadFile(r.etcdCAFile)
	if err != nil {
		return fmt.Errorf("failed to read CA cert: %w", err)
	}

	caKeyData, err := ioutil.ReadFile(r.etcdCAKeyFile)
	if err != nil {
		return fmt.Errorf("failed to read CA key: %w", err)
	}

	certData, keyData, err := tlshelpers.CreateServerCertKey(caCertData, caKeyData, []string{templateData.BootstrapIP})
	if err != nil {
		return err
	}
	err = writeCertKeyFiles(bootstrapManifestsDir, tlshelpers.EtcdAllServingSecretName, tlshelpers.GetServingSecretNameForNode(templateData.Hostname), certData, keyData)
	if err != nil {
		return err
	}

	certData, keyData, err = tlshelpers.CreatePeerCertKey(caCertData, caKeyData, []string{templateData.BootstrapIP})
	if err != nil {
		return err
	}
	err = writeCertKeyFiles(bootstrapManifestsDir, tlshelpers.EtcdAllPeerSecretName, tlshelpers.GetPeerClientSecretNameForNode(templateData.Hostname), certData, keyData)
	if err != nil {
		return err
	}

	return WriteFiles(&r.generic, &templateData.FileConfig, templateData)
}

func writeCertKeyFiles(outputDir, allSecretName, nodeSecretName string, certData, keyData *bytes.Buffer) error {
	dir := path.Join(outputDir, "secrets", allSecretName)

	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create %s directory: %w", allSecretName, err)
	}

	err = ioutil.WriteFile(path.Join(dir, nodeSecretName+".crt"), certData.Bytes(), 0600)
	if err != nil {
		return fmt.Errorf("failed to write %s cert: %w", allSecretName, err)
	}
	err = ioutil.WriteFile(path.Join(dir, nodeSecretName+".key"), keyData.Bytes(), 0600)
	if err != nil {
		return fmt.Errorf("failed to write %s key: %w", allSecretName, err)
	}

	return nil
}

func (t *TemplateData) setBootstrapIP(machineCIDR string, ipv6 bool, excludedIPs []string) error {
	ip, err := defaultBootstrapIPLocator.getBootstrapIP(ipv6, machineCIDR, excludedIPs)
	if err != nil {
		return err
	}
	klog.Infof("using bootstrap IP %s", ip.String())
	t.BootstrapIP = ip.String()
	return nil
}

func (t *TemplateData) setEtcdAddress(ipv6 bool, bootstrapIP string) {
	// IPv4
	allAddresses := "0.0.0.0"
	localhost := "127.0.0.1"

	// IPv6
	if ipv6 {
		allAddresses = "::"
		localhost = "[::1]"
		bootstrapIP = "[" + bootstrapIP + "]"
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

func (t *TemplateData) setMachineCIDR(installConfig map[string]interface{}, ipv6 bool) error {
	cidr, err := getMachineCIDR(installConfig, ipv6)
	if err != nil {
		return err
	}
	if len(cidr) == 0 {
		return fmt.Errorf("no machine CIDR found")
	}
	t.MachineCIDR = cidr
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

func getInstallConfig(clusterConfigMap *unstructured.Unstructured) (map[string]interface{}, error) {
	installConfigYaml, found, err := unstructured.NestedString(clusterConfigMap.Object, "data", "install-config")
	if err != nil {
		return nil, err
	}
	var installConfig map[string]interface{}
	if found {
		installConfigJson, err := yaml.YAMLToJSON([]byte(installConfigYaml))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(installConfigJson, &installConfig); err != nil {
			return nil, fmt.Errorf("failed to unmarshal install config json %w", err)
		}
	}
	return installConfig, nil
}

// getMachineCIDR extracts the machine CIDR from the machineNetwork portion of
// the networking field of the install config. If that doesn't work, tries to
// fall back to the deprecated machineCIDR networking field.
func getMachineCIDR(installConfig map[string]interface{}, isSingleStackIPv6 bool) (string, error) {
	if _, found := installConfig["networking"]; !found {
		return "", fmt.Errorf("install config missing networking key")
	}
	networking, ok := installConfig["networking"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("unrecognized data structure in networking field")
	}

	for _, network := range networking["machineNetwork"].([]interface{})[0].(map[string]interface{}) {
		machineCIDR := fmt.Sprintf("%v", network)
		broadcast, _, err := net.ParseCIDR(machineCIDR)
		if err != nil {
			return "", err
		}
		isIPV4, err := dnshelpers.IsIPv4(broadcast.String())
		if err != nil {
			return "", err
		}
		// IPv4
		if isIPV4 && !isSingleStackIPv6 {
			return machineCIDR, nil
		}
		// IPv6
		if !isIPV4 && isSingleStackIPv6 {
			return machineCIDR, nil
		}
	}

	// check for deprecated definition
	if deprecatedCidr, found := networking["machineCIDR"]; found {
		return fmt.Sprintf("%v", deprecatedCidr), nil
	}

	return "", fmt.Errorf("machineNetwork is not found in install-config")
}

// getExcludedMachineIPs does platform-specific detection of any machine IPs
// which should be excluded from bootstrap IP discovery. On bare metal, this
// means exclude the apiVIP and dnsVIP IPs which can be attached to the same
// interface as the bootstrap IP interface.
//
// In the future, if more metadata were available from the address itself[1]
// about which IPs are virtual, this sort of lookup could perhaps be replaced
// with automatic discovery.
//
// [1] https://elixir.bootlin.com/linux/v4.14/source/include/linux/inetdevice.h#L146
func getExcludedMachineIPs(installConfig map[string]interface{}) ([]string, error) {
	platform, found := installConfig["platform"]
	if !found {
		return nil, fmt.Errorf("install config missing platforn key")
	}
	ips := []string{}
	bm, found := platform.(map[string]interface{})["baremetal"]
	if !found {
		return ips, nil
	}
	for _, key := range []string{"apiVIP", "dnsVIP"} {
		if val, found := bm.(map[string]interface{})[key]; found {
			ips = append(ips, val.(string))
		}
	}
	return ips, nil
}

func (t *TemplateData) setSingleStackIPv6(serviceCIDR []string) error {
	singleStack, err := isSingleStackIPv6(serviceCIDR)
	if err != nil {
		return err
	}
	if singleStack {
		t.SingleStackIPv6 = true
	}
	return nil
}

func (t *TemplateData) setComputedEnvVars(platform string) error {
	envVarMap, err := getEtcdEnv(platform, runtime.GOARCH)
	if err != nil {
		return err
	}
	if len(envVarMap) == 0 {
		return fmt.Errorf("missing env var values")
	}

	envVarLines := []string{}
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		envVarLines = append(envVarLines, fmt.Sprintf("    - name: %q", k))
		envVarLines = append(envVarLines, fmt.Sprintf("      value: %q", v))
	}
	t.ComputedEnvVars = strings.Join(envVarLines, "\n")
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

func getUnstructured(file string) (*unstructured.Unstructured, error) {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	json, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, err
	}
	obj, err := apiruntime.Decode(unstructured.UnstructuredJSONScheme, json)
	if err != nil {
		return nil, err
	}
	config, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("unexpected object in %t", obj)
	}
	return config, nil
}

func getNetwork(file string) (*configv1.Network, error) {
	config := &configv1.Network{}
	yamlData, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	configJson, err := yaml.YAMLToJSON(yamlData)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(configJson, config)
	if err != nil {
		return nil, err
	}
	return config, nil
}

func getInfrastructure(file string) (*configv1.Infrastructure, error) {
	config := &configv1.Infrastructure{}
	yamlData, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	configJson, err := yaml.YAMLToJSON(yamlData)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(configJson, config)
	if err != nil {
		return nil, err
	}
	return config, nil
}
