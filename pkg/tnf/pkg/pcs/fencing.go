package pcs

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const (
	// defaultPcmkDelayBase is the delay applied to the first fence device to prevent simultaneous fencing
	defaultPcmkDelayBase = "10s"
)

type fencingOption int

const (
	Ip fencingOption = iota
	IpPort
	SystemsUri
	Username
	Password
	SslInsecure
	PcmkDelayBase
)

type fencingConfig struct {
	NodeName             string
	FencingID            string
	FencingDeviceType    string
	FencingDeviceOptions map[fencingOption]string
}

// GetParsedIP returns the IP as a string if IP is IPv4 otherwise
// if IP is IPv6 wrap with brackets to be used with port
func (f fencingConfig) GetParsedIP() string {
	if f.FencingDeviceOptions == nil {
		return ""
	}

	ip := net.ParseIP(f.FencingDeviceOptions[Ip])
	if ip == nil {
		return f.FencingDeviceOptions[Ip]
	}
	switch {
	case ip.To4() == nil:
		return fmt.Sprintf("[%s]", ip.String())
	default:
		return ip.String()
	}
}

// ConfigureFencing configures pacemaker fencing based on fencing credentials provided in secrets
func ConfigureFencing(ctx context.Context, kubeClient kubernetes.Interface, cfg config.ClusterConfig) error {
	klog.Info("Setting up pacemaker fencing")

	// get redfish config from secret
	klog.Info("Getting fencing configs from secrets")
	fencingConfigs := []fencingConfig{}

	for i, nodeName := range []string{cfg.NodeName1, cfg.NodeName2} {
		secret, err := tools.GetFencingSecret(ctx, kubeClient, nodeName)
		if err != nil {
			klog.Errorf("Failed to get fencing secret for node %s: %v", nodeName, err)
			return fmt.Errorf("failed to get fencing secret for node %s: %v", nodeName, err)
		}
		fc, err := getFencingConfig(nodeName, secret)
		if err != nil {
			klog.Errorf("Failed to get fencing config for node %s: %v", nodeName, err)
			return fmt.Errorf("failed to get fencing config for node %s: %v", nodeName, err)
		}
		// Add pcmk_delay_base to the first fence device only
		if i == 0 {
			fc.FencingDeviceOptions[PcmkDelayBase] = defaultPcmkDelayBase
		}
		fencingConfigs = append(fencingConfigs, *fc)
	}

	// get current stonith config
	stonithConfig, err := GetStonithConfig(ctx)
	if err != nil {
		klog.Error(err, "Failed to get current stonith config")
		return fmt.Errorf("failed to get current stonith config: %v", err)
	}

	// skip configs for existing stonith devices that are already configured accordingly
	remainingFencingConfigs := make([]fencingConfig, 0)
	for _, fc := range fencingConfigs {
		if canFencingConfigBeSkipped(fc, stonithConfig) {
			klog.Infof("Stonith device for node %s is already configured and does not need an update", fc.NodeName)
		} else {
			remainingFencingConfigs = append(remainingFencingConfigs, fc)
		}
	}

	for _, fc := range remainingFencingConfigs {
		// verify credentials by getting the current BMC status
		klog.Infof("Verifying fencing credentials for node %s", fc.NodeName)
		stdOut, stdErr, err := exec.Execute(ctx, getStatusCommand(fc))
		if err != nil || len(stdErr) > 0 {
			klog.Error(err, "Failed to verify fencing credentials", "stdout", stdOut, "stderr", stdErr, "err", err)
			return fmt.Errorf("failed to verify fencing credentials for node %s: %v", fc.NodeName, err)
		}
		klog.Info(fmt.Sprintf("Fencing credentials for node %s are valid, status command returned %q", fc.NodeName, stdOut))

		// configure stonith devices
		klog.Infof("Configuring pacemaker fencing for node %s", fc.NodeName)
		cmd := getStonithCommand(stonithConfig, fc)
		_, stdErr, err = exec.Execute(ctx, cmd)
		// we use --wait in the stonith create / update command, which makes stdErr to contain "is running" in case the device successfully started
		if err != nil || !strings.Contains(stdErr, "is running") {
			klog.Error(err, "Failed to configure stonith device", "node", fc.NodeName, "stderr", stdErr)
			return fmt.Errorf("failed to configure pacemaker fencing for node %s: %v", fc.NodeName, err)
		}
		klog.Info(fmt.Sprintf("Pacemaker fencing for node %s configured", fc.NodeName))
	}

	// clean up obsolete stonith devices for non-existing nodes
	err = deleteObsoleteStonithDevices(ctx, stonithConfig, fencingConfigs)
	if err != nil {
		klog.Error(err, "Failed to cleanup obsolete stonith devices")
		return fmt.Errorf("failed to cleanup obsolete stonith devices: %v", err)
	}

	klog.Info("Fencing configuration succeeded!")
	return nil

}

func getFencingConfig(nodeName string, secret *corev1.Secret) (*fencingConfig, error) {

	address := string(secret.Data["address"])
	if !strings.Contains(address, "redfish") {
		klog.Errorf("Secret %s does not contain redfish address", secret.Name)
		return nil, fmt.Errorf("secret %s does not contain redfish address", secret.Name)
	}

	// we need to parse ip, port and systems uri from the address like this:
	// redfish+https://192.168.111.1:8000/redfish/v1/Systems/af2167e4-c13b-4941-b606-f912e9a86f4b
	parsedUrl, err := url.Parse(address)
	if err != nil {
		klog.Errorf("Failed to parse redfish address %s", address)
		return nil, fmt.Errorf("failed to parse redfish address %s", address)
	}

	redfishHostname := parsedUrl.Hostname()
	redfishPath := parsedUrl.Path
	redfishPort := parsedUrl.Port()
	// Try to infer standard schema ports for https/http, otherwise notify user port is needed.
	if redfishPort == "" {
		switch {
		case strings.Contains(parsedUrl.Scheme, "https"):
			redfishPort = "443"
		case strings.Contains(parsedUrl.Scheme, "http"):
			redfishPort = "80"
		default:
			klog.Errorf("Failed to parse redfish address, no port number found %s", address)
			return nil, fmt.Errorf("failed to parse redfish address, no port number found %s", address)
		}
	}

	username := string(secret.Data["username"])
	if username == "" {
		klog.Errorf("Secret %s does not contain username", secret.Name)
		return nil, fmt.Errorf("secret %s does not contain username", secret.Name)
	}

	password := string(secret.Data["password"])
	if password == "" {
		klog.Errorf("Secret %s does not contain password", secret.Name)
		return nil, fmt.Errorf("secret %s does not contain password", secret.Name)
	}

	certificateVerification := string(secret.Data["certificateVerification"])
	if certificateVerification == "" {
		klog.Errorf("Secret %s does not contain certificateVerification", secret.Name)
		return nil, fmt.Errorf("secret %s does not contain certificateVerification", secret.Name)
	}

	config := &fencingConfig{
		NodeName:          nodeName,
		FencingID:         fmt.Sprintf("%s_%s", nodeName, "redfish"),
		FencingDeviceType: "fence_redfish",
		FencingDeviceOptions: map[fencingOption]string{
			Ip:         redfishHostname,
			IpPort:     redfishPort,
			SystemsUri: redfishPath,
			Username:   username,
			Password:   password,
		},
	}
	if certificateVerification == "Disabled" {
		config.FencingDeviceOptions[SslInsecure] = ""
	}

	return config, nil
}

func getStatusCommand(fc fencingConfig) string {
	cmd := fmt.Sprintf("/usr/sbin/%s --username %s --password %s --ip %s --ipport %s --systems-uri %s --action status",
		fc.FencingDeviceType, fc.FencingDeviceOptions[Username], fc.FencingDeviceOptions[Password], fc.GetParsedIP(), fc.FencingDeviceOptions[IpPort], fc.FencingDeviceOptions[SystemsUri])

	if _, exists := fc.FencingDeviceOptions[SslInsecure]; exists {
		cmd += " --ssl-insecure"
	}
	return cmd
}

func getStonithCommand(sc StonithConfig, fc fencingConfig) string {

	stonithAction := "create"
	// check if device already exists
	for _, p := range sc.Primitives {
		if p.Id == fc.FencingID {
			stonithAction = "update"
			break
		}
	}

	cmd := fmt.Sprintf("/usr/sbin/pcs stonith %s %s %s username=%q password=%q ip=%q ipport=%q systems_uri=%q pcmk_host_list=%q",
		stonithAction, fc.FencingID, fc.FencingDeviceType, fc.FencingDeviceOptions[Username], fc.FencingDeviceOptions[Password],
		fc.GetParsedIP(), fc.FencingDeviceOptions[IpPort], fc.FencingDeviceOptions[SystemsUri], fc.NodeName)

	if _, exists := fc.FencingDeviceOptions[SslInsecure]; exists {
		cmd += ` ssl_insecure="1"`
	}

	if delayBase, exists := fc.FencingDeviceOptions[PcmkDelayBase]; exists {
		cmd += fmt.Sprintf(` pcmk_delay_base=%q`, delayBase)
	}

	// wait for command execution, so we can check if the device is running
	cmd += " --wait=30"

	return cmd
}

func canFencingConfigBeSkipped(fc fencingConfig, stonithConfig StonithConfig) bool {
	for _, p := range stonithConfig.Primitives {
		if p.Id == fc.FencingID {
			if p.AgentName.Type != fc.FencingDeviceType {
				klog.V(6).Info("agent type needs update")
				return false
			}
			nvPairs := p.InstanceAttributes[0].NvPairs
			if nvPairNeedsUpdate(nvPairs, "pcmk_host_list", fc.NodeName) {
				return false
			}
			for option, value := range fc.FencingDeviceOptions {
				switch option {
				case Ip:
					if nvPairNeedsUpdate(nvPairs, "ip", value) {
						klog.V(1).Info("ip needs update")
						return false
					}
				case IpPort:
					if nvPairNeedsUpdate(nvPairs, "ipport", value) {
						return false
					}
				case Username:
					if nvPairNeedsUpdate(nvPairs, "username", value) {
						return false
					}
				case Password:
					if nvPairNeedsUpdate(nvPairs, "password", value) {
						return false
					}
				case SystemsUri:
					if nvPairNeedsUpdate(nvPairs, "systems_uri", value) {
						return false
					}
				case SslInsecure:
					if nvPairNeedsUpdate(nvPairs, "ssl_insecure", "1") {
						return false
					}
				case PcmkDelayBase:
					if nvPairNeedsUpdate(nvPairs, "pcmk_delay_base", value) {
						return false
					}
				}
			}
			return true
		}
	}
	return false
}

func nvPairNeedsUpdate(nvPairs []NvPair, name string, value string) bool {
	for _, nvPair := range nvPairs {
		if nvPair.Name == name && nvPair.Value == value {
			return false
		}
	}
	return true
}

func deleteObsoleteStonithDevices(ctx context.Context, stonithConfig StonithConfig, fencingConfigs []fencingConfig) error {
	for _, p := range stonithConfig.Primitives {
		found := false
		for _, fc := range fencingConfigs {
			if p.Id == fc.FencingID {
				found = true
				break
			}
		}
		if found {
			continue
		}

		klog.Infof("Deleting obsolete stonith device %q", p.Id)
		stdOut, stdErr, err := exec.Execute(ctx, fmt.Sprintf("/usr/sbin/pcs stonith delete %s", p.Id))
		// stderr contains a warning!
		if err != nil {
			klog.Error(err, "Failed to delete stonith device", "stdout", stdOut, "stderr", stdErr)
			return fmt.Errorf("failed to delete stonith device %q: %v", p.Id, err)
		}
	}
	return nil
}
