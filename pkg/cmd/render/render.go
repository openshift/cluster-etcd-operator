package render

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	corev1 "k8s.io/api/core/v1"

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
	fs.StringVar(&r.templateDir, "template-dir", "/usr/share/bootkube/manifests", "Root folder for bootkube manifests, in this repo stored under bindata/bootkube")
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

	// ClusterCIDR is the IP range for pod IPs.
	ClusterCIDR []string

	// ServiceCIDR is the IP range for service IPs.
	ServiceCIDR []string

	// MachineCIDR is the IP range for machine IPs.
	MachineCIDR string

	// PreferIPv6 is true if IPv6 is the primary IP version.
	PreferIPv6 bool

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

	// MaxLearners is the maximum number of learner members that can be part of the cluster membership.
	MaxLearners int

	// Optional Additional platform information (ex. ProviderType for IBMCloud).
	PlatformData string

	// not intended for templating, but written directly as a resource
	certificates []corev1.Secret
	caBundles    []corev1.ConfigMap
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

	if err := templateData.setPreferIPv6(templateData.ServiceCIDR); err != nil {
		return nil, err
	}
	if err := templateData.setMachineCIDR(installConfig, templateData.PreferIPv6); err != nil {
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
	if err := templateData.setBootstrapIP(templateData.MachineCIDR, templateData.PreferIPv6, excludedIPs); err != nil {
		return nil, err
	}

	templateData.setEtcdAddress(templateData.PreferIPv6, templateData.BootstrapIP)

	// assume that this is >4.2
	templateData.Platform = string(infra.Status.PlatformStatus.Type)
	switch infra.Status.PlatformStatus.Type {
	case configv1.IBMCloudPlatformType:
		templateData.PlatformData = string(infra.Status.PlatformStatus.IBMCloud.ProviderType)
	}

	if err := templateData.setBootstrapStrategy(installConfig, opts.delayedHABootstrapScalingStrategyMarker); err != nil {
		return nil, err
	}

	if err := templateData.setComputedEnvVars(templateData.Platform, templateData.PlatformData, installConfig); err != nil {
		return nil, err
	}

	// If bootstrap scaling strategy is delayed HA set annotation signal
	if templateData.BootstrapScalingStrategy == ceohelpers.DelayedHAScalingStrategy ||
		templateData.BootstrapScalingStrategy == ceohelpers.DelayedTwoNodeScalingStrategy {
		templateData.NamespaceAnnotations = map[string]string{
			ceohelpers.DelayedBootstrapScalingStrategyAnnotation: "",
		}
	}

	// If bootstrap scaling strategy is in place set endpoint data
	if templateData.BootstrapScalingStrategy == ceohelpers.BootstrapInPlaceStrategy {
		// base64 encode the ip address and use it as unique data key to emulate endpoint controller
		templateData.EtcdEndpointConfigmapData = fmt.Sprintf("%s: %s",
			base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(templateData.BootstrapIP)), templateData.BootstrapIP)
	}

	enabledFeatureGates, disabledFeatureGates := getFeatureGatesStatus(installConfig)

	certs, bundles, err := createBootstrapCertSecrets(templateData.Hostname, templateData.BootstrapIP, enabledFeatureGates, disabledFeatureGates)
	if err != nil {
		return nil, err
	}

	templateData.certificates = certs
	templateData.caBundles = bundles

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
	// tlsDir contains the ca bundle and client cert pair for bootkube.sh and the bootstrap apiserver
	tlsDir := filepath.Join(r.assetOutputDir, "tls")
	err = os.MkdirAll(tlsDir, 0755)

	// Write the etcd ca bundle required by the bootstrap etcd member
	for _, bundle := range templateData.caBundles {
		if bundle.Name == tlshelpers.EtcdSignerCaBundleConfigMapName {
			err = writeCertFile(memberDir, "ca", []byte(bundle.Data["ca-bundle.crt"]))
			if err != nil {
				return err
			}

			// this is the bundle for the bootstrapping apiserver, stored in a special folder
			err = writeCertFile(tlsDir, "etcd-ca-bundle", []byte(bundle.Data["ca-bundle.crt"]))
			if err != nil {
				return err
			}
			break
		}
	}

	// Write the etcd certificates required by the bootstrap etcd member
	for _, certSecret := range templateData.certificates {
		if certSecret.Name == tlshelpers.EtcdAllCertsSecretName {
			continue
		}

		err = writeCertKeyFiles(certDir, certSecret.Name, certSecret.Data["tls.crt"], certSecret.Data["tls.key"])
		if err != nil {
			return err
		}

		// the client cert is stored in a special folder for the bootstrapping apiserver
		if certSecret.Name == tlshelpers.EtcdClientCertSecretName {
			err = writeCertKeyFiles(tlsDir, "etcd-client", certSecret.Data["tls.crt"], certSecret.Data["tls.key"])
			if err != nil {
				return err
			}
		}
	}

	err = writeManifests(r.assetOutputDir, r.templateDir, templateData)
	if err != nil {
		return err
	}

	// the certificates are not templated anymore, so we directly write those to the right folder
	for _, cert := range templateData.certificates {
		destinationPath := filepath.Join(r.assetOutputDir, "manifests", fmt.Sprintf("%s-%s.yaml", cert.Namespace, cert.Name))
		bytes, err := json.Marshal(cert)
		if err != nil {
			return fmt.Errorf("error while marshalling secret %s: %w", cert.Name, err)
		}
		bytes, err = yaml.JSONToYAML(bytes)
		if err != nil {
			return fmt.Errorf("error while converting secret from json to yaml %s: %w", cert.Name, err)
		}
		err = os.WriteFile(destinationPath, bytes, 0644)
		if err != nil {
			return fmt.Errorf("error while writing secret %s: %w", cert.Name, err)
		}
	}

	for _, bundle := range templateData.caBundles {
		destinationPath := filepath.Join(r.assetOutputDir, "manifests", fmt.Sprintf("%s-%s.yaml", bundle.Namespace, bundle.Name))
		bytes, err := json.Marshal(bundle)
		if err != nil {
			return fmt.Errorf("error while marshalling cm %s: %w", bundle.Name, err)
		}
		bytes, err = yaml.JSONToYAML(bytes)
		if err != nil {
			return fmt.Errorf("error while converting cm from json to yaml %s: %w", bundle.Name, err)
		}
		err = os.WriteFile(destinationPath, bytes, 0644)
		if err != nil {
			return fmt.Errorf("error while writing cm %s: %w", bundle.Name, err)
		}
	}

	return nil
}

func writeCertKeyFiles(dir, name string, certData, keyData []byte) error {
	err := writeCertFile(dir, name, certData)
	if err != nil {
		return err
	}
	return writeKeyFile(dir, name, keyData)
}

func writeCertFile(dir, name string, certData []byte) error {
	err := os.WriteFile(path.Join(dir, name+".crt"), certData, 0600)
	if err != nil {
		return fmt.Errorf("failed to write %s cert: %w", name, err)
	}
	return nil
}

func writeKeyFile(dir, name string, keyData []byte) error {
	err := os.WriteFile(path.Join(dir, name+".key"), keyData, 0600)
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
func getMachineCIDR(installConfig map[string]interface{}, preferIPv6 bool) (string, error) {
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
		if isIPV4 && !preferIPv6 {
			return machineCIDR, nil
		}
		// IPv6
		if !isIPV4 && preferIPv6 {
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

func (t *TemplateData) setPreferIPv6(serviceCIDR []string) error {
	preferIPv6, err := preferIPv6(serviceCIDR)
	if err != nil {
		return err
	}
	if preferIPv6 {
		t.PreferIPv6 = true
	}
	return nil
}

func (t *TemplateData) setComputedEnvVars(platform, platformData string, installConfig map[string]interface{}) error {
	envVarData := &envVarData{
		platform:         platform,
		platformData:     platformData,
		arch:             runtime.GOARCH,
		installConfig:    installConfig,
		hostname:         t.Hostname,
		inPlaceBootstrap: t.BootstrapScalingStrategy == ceohelpers.BootstrapInPlaceStrategy,
	}
	envVarMap, err := getEtcdEnv(envVarData)
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
		manifests, err := assets.New(filepath.Join(templateDir, manifestDir), templateData, []assets.FileContentsPredicate{}, assets.OnlyYaml)
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

func preferIPv6(serviceCIDRs []string) (bool, error) {
	if len(serviceCIDRs) == 0 {
		return false, fmt.Errorf("preferIPv6: no serviceCIDRs passed")
	}
	// We can tell the desired bootstrap IP version based on the version of
	// the first CIDR. For example, in a single stack IPv4 cluster the CIDR
	// list will look something like:
	// 10.0.0.0/16, 10.1.0.0/16, 10.2.0.0/16, etc.
	// which means we want IPv4. Similarly, for single stack IPv6:
	// fd00::/64, fe00::/64, ff00::/64, etc.
	// In dual stack, the ordering of the CIDRs indicates which IP version
	// should be considered primary:
	// 10.0.0.0/16, fd00::/64
	// is v4-primary while
	// fd00::/64, 10.0.0.0/16
	// is v6-primary. It doesn't matter how many CIDRs are in the list, the
	// first one must be primary.
	// The bootstrap IP must match the primary IP version because the
	// cluster nodes are going to select an address from the primary version
	// and if that doesn't match there will be connectivity issues back to
	// the boostrap.
	ip, _, err := net.ParseCIDR(serviceCIDRs[0])
	if err != nil {
		return false, err
	}
	if ip.To4() != nil {
		return false, nil
	}
	return true, nil
}

func getUnstructured(file string) (*unstructured.Unstructured, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	jsonBytes, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, err
	}
	obj, err := apiruntime.Decode(unstructured.UnstructuredJSONScheme, jsonBytes)
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
	yamlData, err := os.ReadFile(file)
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
	yamlData, err := os.ReadFile(file)
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
	controlPlane, found := installConfig["controlPlane"].(map[string]interface{})
	if !found {
		return "", fmt.Errorf("unrecognized data structure in controlPlane field")
	}
	cpReplicaCount, found := controlPlane["replicas"].(float64)
	if !found {
		return "", fmt.Errorf("unrecognized data structure in controlPlane replica field")
	}

	// Bootstrap in place strategy when bootstrapInPlace root key exists in the install-config
	// and controlPlane replicas is 1.
	if _, found := installConfig["bootstrapInPlace"]; found && int(cpReplicaCount) == 1 {
		return ceohelpers.BootstrapInPlaceStrategy, nil
	}

	// Delayed HA strategy is set if marker file exists on disk.
	_, err := os.Stat(delayedHAMarkerFile)
	delayedHAEnabled := err == nil

	// Check for arbiter to distinguish between Two Node OpenShift with
	// Fencing and Two Node OpenShift with Arbiter
	arbReplicaCount := int64(0)
	arbiterConfig, arbiterDefined := installConfig["arbiter"].(map[string]interface{})
	if arbiterDefined {
		replicas, arbReplicasDefined := arbiterConfig["replicas"].(float64)
		if arbReplicasDefined {
			arbReplicaCount = int64(replicas)
		}
	}

	strategy := ceohelpers.HAScalingStrategy
	switch {
	// None of this logic is used.
	// The only code that references the scaling strategy written to the
	// manifests lives in the 00_etcd-endpoints-cm.yaml and etcd-member-pod.yaml
	// templates, which just check for BootstrapInPlaceStrategy (see logic above).
	case cpReplicaCount == 2 && arbReplicaCount == 0 && delayedHAEnabled:
		strategy = ceohelpers.DelayedTwoNodeScalingStrategy
	case cpReplicaCount == 2 && arbReplicaCount == 0 && !delayedHAEnabled:
		strategy = ceohelpers.TwoNodeScalingStrategy
	case delayedHAEnabled:
		strategy = ceohelpers.DelayedHAScalingStrategy
	case cpReplicaCount == 1:
		strategy = ceohelpers.UnsafeScalingStrategy
	default:
		strategy = ceohelpers.HAScalingStrategy
	}

	// HA "default".
	return strategy, nil
}

// getFeatureGatesStatus returns the enabled and disabled feature gates.
func getFeatureGatesStatus(installConfig map[string]interface{}) (sets.Set[configv1.FeatureGateName], sets.Set[configv1.FeatureGateName]) {
	enabled, disabled := sets.Set[configv1.FeatureGateName]{}, sets.Set[configv1.FeatureGateName]{}

	// On bootstrap we might not be able to fetch a list of all feature gates
	// Hardcode a list of necessary for bootstrap here
	necessaryFeatureGates := []configv1.FeatureGateName{"ShortCertRotation"}
	disabled.Insert(necessaryFeatureGates...)

	featureGates, found := installConfig["featureGates"]
	if !found {
		return enabled, disabled
	}

	for _, featureGate := range featureGates.([]interface{}) {
		key := strings.Split(featureGate.(string), "=")[0]
		value := strings.Split(featureGate.(string), "=")[1]
		if value == "true" {
			enabled = enabled.Insert(configv1.FeatureGateName(key))
			disabled = disabled.Delete(configv1.FeatureGateName(key))
		} else if value == "false" {
			enabled = enabled.Delete(configv1.FeatureGateName(key))
			disabled = disabled.Insert(configv1.FeatureGateName(key))
		}
	}

	return enabled, disabled
}
