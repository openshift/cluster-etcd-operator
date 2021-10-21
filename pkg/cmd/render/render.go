package render

import (
	"encoding/base64"
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
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"

	"github.com/ghodss/yaml"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/assets"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// renderOpts holds values to drive the render command.
type renderOpts struct {
	// Path to render assets to
	assetOutputDir string

	// Path containing resource templates to render
	templateDir string

	errOut               io.Writer
	etcdImage            string
	networkConfigFile    string
	clusterConfigMapFile string
	infraConfigFile      string

	delayedHABootstrapScalingStrategyMarker string
}

// NewRenderCommand creates a render command.
func NewRenderCommand(errOut io.Writer) *cobra.Command {
	renderOpts := renderOpts{
		errOut: errOut,
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
			must(renderOpts.Run)
		},
	}

	renderOpts.AddFlags(cmd.Flags())

	return cmd
}

func (r *renderOpts) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&r.assetOutputDir, "asset-output-dir", "", "Output path for rendered assets.")
	fs.StringVar(&r.etcdImage, "etcd-image", "", "etcd image to use for bootstrap.")
	fs.StringVar(&r.networkConfigFile, "network-config-file", "", "File containing the network.config.openshift.io manifest.")
	fs.StringVar(&r.clusterConfigMapFile, "cluster-configmap-file", "", "File containing the cluster-config-v1 configmap.")
	fs.StringVar(&r.infraConfigFile, "infra-config-file", "", "File containing infrastructure.config.openshift.io manifest.")

	// TODO(marun) Discover scaling strategy with less hack
	fs.StringVar(&r.delayedHABootstrapScalingStrategyMarker, "delayed-ha-bootstrap-scaling-marker-file", "/assets/assisted-install-bootstrap", "Marker file that, if present, enables the delayed HA bootstrap scaling strategy")

}

// Validate verifies the inputs.
func (r *renderOpts) Validate() error {
	if len(r.assetOutputDir) == 0 {
		return errors.New("missing required flag: --asset-output-dir")
	}
	if len(r.etcdImage) == 0 {
		return errors.New("missing required flag: --etcd-image")
	}
	if len(r.infraConfigFile) == 0 {
		return errors.New("missing required flag: --infra-config-file")
	}
	if len(r.networkConfigFile) == 0 {
		return errors.New("missing required flag: --network-config-file")
	}
	if len(r.clusterConfigMapFile) == 0 {
		return errors.New("missing required flag: --cluster-configmap-file")
	}
	return nil
}

// etcdAddress collects addresses used to populate the etcd static
// pod spec
type etcdAddress struct {
	ListenClient       string
	ListenPeer         string
	ListenMetricServer string
	ListenMetricProxy  string
	LocalHost          string
	EscapedBootstrapIP string
}

type TemplateData struct {
	// Pull spec for the bootstrap etcd pod
	Image string

	// Addresses for static pod spec
	EtcdAddress etcdAddress

	EtcdServerCertDNSNames string
	EtcdPeerCertDNSNames   string

	// CA
	EtcdSignerCert []byte
	EtcdSignerKey  []byte

	// CA bundle
	EtcdCaBundle []byte

	// Client cert
	EtcdSignerClientCert []byte
	EtcdSignerClientKey  []byte

	// Metrics CA
	EtcdMetricSignerCert []byte
	EtcdMetricSignerKey  []byte

	// Metrics CA bundle
	EtcdMetricCaBundle []byte

	// Metrics client cert
	EtcdMetricSignerClientCert []byte
	EtcdMetricSignerClientKey  []byte

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

	// BootstrapScalingStrategy describes the invariants which will be enforced when
	// scaling the etcd cluster.
	ceohelpers.BootstrapScalingStrategy

	// Platform is the underlying provider the cluster is run on.
	Platform string

	// ComputedEnvVars name/value pairs to populate env: for static pod.
	ComputedEnvVars string

	// NamespaceAnnotations are addition annotations to apply to the etcd namespace.
	NamespaceAnnotations map[string]string

	// EtcdEndpointConfigmapData is an optional data field used by etcd-endpoints configmap.
	EtcdEndpointConfigmapData string
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
		Image: opts.etcdImage,
		EtcdServerCertDNSNames: strings.Join([]string{
			"localhost",
			"etcd.kube-system.svc",
			"etcd.kube-system.svc.cluster.local",
			"etcd.openshift-etcd.svc",
			"etcd.openshift-etcd.svc.cluster.local",
		}, ","),
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

	// Set the bootstrap ip
	excludedIPs, err := getExcludedMachineIPs(installConfig)
	if err != nil {
		return nil, err
	}
	if err := templateData.setBootstrapIP(templateData.MachineCIDR, templateData.SingleStackIPv6, excludedIPs); err != nil {
		return nil, err
	}

	templateData.setEtcdAddress(templateData.SingleStackIPv6, templateData.BootstrapIP)

	// assume that this is >4.2
	templateData.Platform = string(infra.Status.PlatformStatus.Type)

	if err := templateData.setBootstrapStrategy(installConfig, opts.delayedHABootstrapScalingStrategyMarker); err != nil {
		return nil, err
	}

	if err := templateData.setComputedEnvVars(templateData.Platform); err != nil {
		return nil, err
	}

	// If bootstrap scaling strategy is delayed HA set annotation signal
	if templateData.BootstrapScalingStrategy == ceohelpers.DelayedHAScalingStrategy {
		templateData.NamespaceAnnotations = map[string]string{
			ceohelpers.DelayedHABootstrapScalingStrategyAnnotation: "",
		}
	}

	// If bootstrap scaling strategy is in place set endpoint data
	if templateData.BootstrapScalingStrategy == ceohelpers.BootstrapInPlaceStrategy {
		// base64 encode the ip address and use it as unique data key to emulate endpoint controller
		templateData.EtcdEndpointConfigmapData = fmt.Sprintf("%s: %s",
			base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(templateData.BootstrapIP)), templateData.BootstrapIP)
	}

	// Signer key material
	caKeyMaterial, err := createKeyMaterial("etcd-signer", "etcd")
	if err != nil {
		return nil, err
	}
	templateData.EtcdSignerCert = caKeyMaterial.caCert
	templateData.EtcdSignerKey = caKeyMaterial.caKey
	templateData.EtcdCaBundle = caKeyMaterial.caBundle
	templateData.EtcdSignerClientCert = caKeyMaterial.clientCert
	templateData.EtcdSignerClientKey = caKeyMaterial.clientKey

	// Metric key material
	metricKeyMaterial, err := createKeyMaterial("etcd-metric-signer", "etcd-metric")
	if err != nil {
		return nil, err
	}
	templateData.EtcdMetricSignerCert = metricKeyMaterial.caCert
	templateData.EtcdMetricSignerKey = metricKeyMaterial.caKey
	templateData.EtcdMetricCaBundle = metricKeyMaterial.caBundle
	templateData.EtcdMetricSignerClientCert = metricKeyMaterial.clientCert
	templateData.EtcdMetricSignerClientKey = metricKeyMaterial.clientKey

	return &templateData, nil
}

// Run contains the logic of the render command.
func (r *renderOpts) Run() error {
	templateData, err := newTemplateData(r)
	if err != nil {
		return err
	}

	// Set the template dir only if not already set to support
	// overriding the path in unit tests.
	if len(r.templateDir) == 0 {
		r.templateDir = "/usr/share/bootkube/manifests"
	}

	// Base path for bootstrap configuration
	etcKubernetesDir := filepath.Join(r.assetOutputDir, "etc-kubernetes")

	// Path for bootstrap etcd member configuration
	memberDir := filepath.Join(etcKubernetesDir, "static-pod-resources", "etcd-member")

	// Path for certs used by the bootstrap etcd member
	certDir := filepath.Join(memberDir, "etcd-all-certs")

	// Creating the cert dir recursively will create the base path too
	err = os.MkdirAll(certDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create directory %s: %w", memberDir, err)
	}

	// Write the ca bundle required by the bootstrap etcd member
	err = writeCertFile(memberDir, "ca", []byte(templateData.EtcdCaBundle))
	if err != nil {
		return err
	}

	// Write the serving and peer certs for the bootstrap etcd member
	caCertData := templateData.EtcdSignerCert
	caKeyData := templateData.EtcdSignerKey
	serverCertData, serverKeyData, err := tlshelpers.CreateServerCertKey(caCertData, caKeyData, []string{templateData.BootstrapIP})
	if err != nil {
		return err
	}
	err = writeCertKeyFiles(certDir, tlshelpers.GetServingSecretNameForNode(templateData.Hostname), serverCertData.Bytes(), serverKeyData.Bytes())
	if err != nil {
		return err
	}
	peerCertData, peerKeyData, err := tlshelpers.CreatePeerCertKey(caCertData, caKeyData, []string{templateData.BootstrapIP})
	if err != nil {
		return err
	}
	err = writeCertKeyFiles(certDir, tlshelpers.GetPeerClientSecretNameForNode(templateData.Hostname), peerCertData.Bytes(), peerKeyData.Bytes())
	if err != nil {
		return err
	}

	// Write the ca bundle and client cert pair for bootkube.sh and the bootstrap apiserver
	tlsDir := filepath.Join(r.assetOutputDir, "tls")
	err = os.MkdirAll(tlsDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create directory %s: %w", tlsDir, err)
	}
	err = writeCertFile(tlsDir, "etcd-ca-bundle", []byte(templateData.EtcdCaBundle))
	if err != nil {
		return err
	}
	err = writeCertKeyFiles(tlsDir, "etcd-client", templateData.EtcdSignerClientCert, templateData.EtcdSignerClientKey)
	if err != nil {
		return err
	}

	return writeManifests(r.assetOutputDir, r.templateDir, templateData)
}

func writeCertKeyFiles(dir, name string, certData, keyData []byte) error {
	err := writeCertFile(dir, name, certData)
	if err != nil {
		return err
	}
	return writeKeyFile(dir, name, keyData)
}

func writeCertFile(dir, name string, certData []byte) error {
	err := ioutil.WriteFile(path.Join(dir, name+".crt"), certData, 0600)
	if err != nil {
		return fmt.Errorf("failed to write %s cert: %w", name, err)
	}
	return nil
}

func writeKeyFile(dir, name string, keyData []byte) error {
	err := ioutil.WriteFile(path.Join(dir, name+".key"), keyData, 0600)
	if err != nil {
		return fmt.Errorf("failed to write %s key: %w", name, err)
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

	t.EtcdAddress = etcdAddress{
		ListenClient:       net.JoinHostPort(allAddresses, "2379"),
		ListenPeer:         net.JoinHostPort(allAddresses, "2380"),
		LocalHost:          localhost,
		ListenMetricServer: net.JoinHostPort(allAddresses, "9978"),
		ListenMetricProxy:  net.JoinHostPort(allAddresses, "9979"),
		EscapedBootstrapIP: bootstrapIP,
	}
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

	for _, machineNetwork := range networking["machineNetwork"].([]interface{}) {
		network := machineNetwork.(map[string]interface{})
		machineCIDR := fmt.Sprintf("%v", network["cidr"])
		if len(machineCIDR) == 0 {
			return "", fmt.Errorf("malformed machineNetwork entry is missing the cidr field: %#v", network)
		}
		broadcast, _, err := net.ParseCIDR(machineCIDR)
		if err != nil {
			return "", err
		}

		// Placeholder IP addresses are not allowed for machine CIDR as bootstrap will fail
		// due to the endpoints being skipped over by the config obeservers
		// See: https://github.com/openshift/cluster-kube-apiserver-operator/blob/37b25afec3e8666af6b668e979cc9299ccce62d4/pkg/operator/configobservation/etcdendpoints/observe_etcd_endpoints.go#L72
		// TODO(hasbro17): Use a set for all reserved/not-allowed IPv4/6 addresses?
		if strings.HasPrefix(broadcast.String(), "192.0.2.") || strings.HasPrefix(broadcast.String(), "2001:db8:") {
			return "", fmt.Errorf("machineNetwork CIDR is reserved and unsupported: %q", broadcast.String())
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

func (t *TemplateData) setBootstrapStrategy(installConfig map[string]interface{}, delayedHAMarkerFile string) error {
	bootstrapStrategy, err := getBootstrapScalingStrategy(installConfig, delayedHAMarkerFile)
	if err != nil {
		return err
	}
	klog.Infof("Bootstrapping etcd using: %q", bootstrapStrategy)
	t.BootstrapScalingStrategy = bootstrapStrategy
	return nil
}

func writeManifests(outputDir, templateDir string, templateData interface{}) error {
	assetPaths := map[string]string{
		// The etc-kubernetes path will be copied recursively by the installer to /etc/kubernetes/
		"bootstrap-manifests": filepath.Join("etc-kubernetes", "manifests"),
		// The contents of the manifests path will be copied by the installer to manifests/
		"manifests": "manifests",
	}

	for manifestDir, destinationDir := range assetPaths {
		manifests, err := assets.New(filepath.Join(templateDir, manifestDir), templateData, assets.OnlyYaml)
		if err != nil {
			return fmt.Errorf("failed rendering assets: %v", err)
		}
		destinationPath := filepath.Join(outputDir, destinationDir)
		if err := manifests.WriteFiles(destinationPath); err != nil {
			return fmt.Errorf("failed writing assets to %q: %v", destinationPath, err)
		}
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

func getBootstrapScalingStrategy(installConfig map[string]interface{}, delayedHAMarkerFile string) (ceohelpers.BootstrapScalingStrategy, error) {
	// Delayed HA strategy is set if marker file exists on disk.
	if _, err := os.Stat(delayedHAMarkerFile); err == nil {
		return ceohelpers.DelayedHAScalingStrategy, nil
	}

	controlPlane, found := installConfig["controlPlane"].(map[string]interface{})
	if !found {
		return "", fmt.Errorf("unrecognized data structure in controlPlane field")
	}
	replicaCount, found := controlPlane["replicas"].(float64)
	if !found {
		return "", fmt.Errorf("unrecognized data structure in controlPlane replica field")
	}

	// Bootstrap in place strategy when bootstrapInPlace root key exists in the install-config
	// and controlPlane replicas is 1.
	if _, found := installConfig["bootstrapInPlace"]; found && int(replicaCount) == 1 {
		return ceohelpers.BootstrapInPlaceStrategy, nil
	}

	// HA "default".
	return ceohelpers.HAScalingStrategy, nil
}
