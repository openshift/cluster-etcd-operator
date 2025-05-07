package pcs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

const (
	secretNamePattern = "fencing-credentials-%s"
)

var (
	addressRegEx = regexp.MustCompile(`.*//(.*):(.*)(/redfish.*)`)
)

type fencingOption int

const (
	Ip fencingOption = iota
	IpPort
	SystemsUri
	Username
	Password
	SslInsecure
)

type fencingConfig struct {
	NodeName             string
	FencingID            string
	FencingDeviceType    string
	FencingDeviceOptions map[fencingOption]string
}

// ConfigureFencing configures pacemaker fencing based on fencing credentials provided in secrets
func ConfigureFencing(ctx context.Context, kubeClient kubernetes.Interface, cfg config.ClusterConfig) (err error) {
	klog.Info("Setting up pacemaker fencing")

	// disable stonith in case things fail
	defer func() {
		// this only works because we named the returned error to "err"
		if err != nil {
			klog.Error("Failed to configure fencing, cleaning up")
			cleanupErr := disableFencing(ctx)
			if cleanupErr != nil {
				klog.Errorf("Failed to cleanup fencing configuration: %v", cleanupErr)
				err = errors.Join(err, cleanupErr)
			}
		}
	}()

	klog.Info("Cleaning up old fencing config")
	err = disableFencing(ctx)
	if err != nil {
		klog.Errorf("Failed to clean up old fencing config: %v", err)
		return fmt.Errorf("failed to clean up old fencing config: %v", err)
	}

	// get redfish config from secret
	klog.Info("Getting fencing configs from secrets")
	fencingConfigs := []fencingConfig{}

	for _, nodeName := range []string{cfg.NodeName1, cfg.NodeName2} {
		secretName := fmt.Sprintf(secretNamePattern, nodeName)
		secret, err := getSecret(ctx, kubeClient, secretName)
		if err != nil {
			klog.Errorf("Failed to get secret %s: %v", secretName, err)
			return fmt.Errorf("failed to get secret %s: %v", secretName, err)
		}
		fc, err := getFencingConfig(nodeName, secret)
		if err != nil {
			klog.Errorf("Failed to get fencing config for node %s: %v", nodeName, err)
			return fmt.Errorf("failed to get fencing config for node %s: %v", nodeName, err)
		}
		fencingConfigs = append(fencingConfigs, *fc)
	}

	// verify credentials by getting the current BMC status
	for _, fc := range fencingConfigs {
		klog.Infof("Verifying fencing credentials for node %s", fc.NodeName)
		stdOut, stdErr, err := exec.Execute(ctx, getStatusCommand(fc))
		if err != nil || len(stdErr) > 0 {
			klog.Error(err, "Failed to verify fencing credentials", "stdout", stdOut, "stderr", stdErr, "err", err)
			return fmt.Errorf("failed to verify fencing credentials for node %s: %v", fc.NodeName, err)
		}
		klog.Info(fmt.Sprintf("Fencing credentials for node %s are valid, status command returned %q", fc.NodeName, stdOut))
	}

	// enable stonith
	stdOut, stdErr, err := exec.Execute(ctx, "/usr/sbin/pcs property set stonith-enabled=true")
	if err != nil || len(stdErr) > 0 {
		klog.Error(err, "Failed to enable stonith", "stdout", stdOut, "stderr", stdErr, "err", err)
		return fmt.Errorf("failed to enable stonith: %v", err)
	}

	// configure stonith devices
	for _, fc := range fencingConfigs {
		klog.Infof("Configuring pacemaker fencing for node %s", fc.NodeName)
		stdOut, stdErr, err = exec.Execute(ctx, getStonithCommand(fc))
		if err != nil || len(stdErr) > 0 {
			klog.Error(err, "Failed to create stonith device", "stdout", stdOut, "stderr", stdErr, "err", err)
			return fmt.Errorf("failed to configure pacemaker fencing for node %s: %v", fc.NodeName, err)
		}
		klog.Info(fmt.Sprintf("Pacemaker fencing for node %s configured", fc.NodeName))
	}

	klog.Info("All fencing configuration succeeded!")
	return nil

}

func disableFencing(ctx context.Context) error {
	klog.Info("Disabling pacemaker fencing")
	stdOut, stdErr, err := exec.Execute(ctx, "/usr/sbin/pcs property set stonith-enabled=false")
	if err != nil || len(stdErr) > 0 {
		klog.Error(err, "Failed to disable stonith", "stdout", stdOut, "stderr", stdErr, "err", err)
		return fmt.Errorf("failed to disable stonith: %v", err)
	}
	klog.Info("Pacemaker fencing disabled")

	return deleteStonithDevices(ctx)
}

func deleteStonithDevices(ctx context.Context) error {
	klog.Info("Getting stonith config for device deletion")
	stdOut, stdErr, err := exec.Execute(ctx, "/usr/sbin/pcs stonith config --output-format json")
	// stderr contains a warning!
	if err != nil {
		klog.Error(err, "Failed to get stonith config", "stdout", stdOut, "stderr", stdErr, "err", err)
		return fmt.Errorf("failed to get stonith config: %v", err)
	}
	stonithConfig, err := UnmarshalStonithConfig(stdOut)
	if err != nil {
		klog.Error(err, "Failed to unmarshal stonith config", "stdout", stdOut, "stderr", stdErr, "err", err)
		return fmt.Errorf("failed to unmarshal stonith config: %v", err)
	}
	for _, p := range stonithConfig.Primitives {
		klog.Info("Deleting stonith device %q", p.Id)
		stdOut, stdErr, err = exec.Execute(ctx, fmt.Sprintf("/usr/sbin/pcs stonith delete %s", p.Id))
		// stderr contains a warning!
		if err != nil {
			klog.Error(err, "Failed to delete stonith device", "stdout", stdOut, "stderr", stdErr, "err", err)
			return fmt.Errorf("failed to delete stonith device %q: %v", p.Id, err)
		}
		klog.Info(fmt.Sprintf("Pacemaker fencing device %q deleted", p.Id))
	}
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
	matches := addressRegEx.FindStringSubmatch(address)
	if len(matches) != 4 {
		klog.Errorf("Failed to parse redfish address %s", address)
		return nil, fmt.Errorf("failed to parse redfish address %s", address)
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
			Ip:         matches[1],
			IpPort:     matches[2],
			SystemsUri: matches[3],
			Username:   username,
			Password:   password,
		},
	}
	if certificateVerification == "Disabled" {
		config.FencingDeviceOptions[SslInsecure] = ""
	}

	return config, nil
}

func getSecret(ctx context.Context, kubeClient kubernetes.Interface, secretName string) (*corev1.Secret, error) {
	secret, err := kubeClient.CoreV1().Secrets("openshift-etcd").Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get secret %s: %v", secretName, err)
		return nil, err
	}
	return secret, nil
}

func getStatusCommand(fc fencingConfig) string {
	cmd := fmt.Sprintf("/usr/sbin/%s --username %s --password %s --ip %s --ipport %s --systems-uri %s --action status",
		fc.FencingDeviceType, fc.FencingDeviceOptions[Username], fc.FencingDeviceOptions[Password], fc.FencingDeviceOptions[Ip], fc.FencingDeviceOptions[IpPort], fc.FencingDeviceOptions[SystemsUri])

	if _, exists := fc.FencingDeviceOptions[SslInsecure]; exists {
		cmd += " --ssl-insecure"
	}
	return cmd
}

func getStonithCommand(fc fencingConfig) string {
	cmd := fmt.Sprintf("/usr/sbin/pcs stonith create %s %s username=%q password=%q ip=%q ipport=%q systems_uri=%q pcmk_host_list=%q",
		fc.FencingID, fc.FencingDeviceType, fc.FencingDeviceOptions[Username], fc.FencingDeviceOptions[Password],
		fc.FencingDeviceOptions[Ip], fc.FencingDeviceOptions[IpPort], fc.FencingDeviceOptions[SystemsUri], fc.NodeName)

	if _, exists := fc.FencingDeviceOptions[SslInsecure]; exists {
		cmd += ` ssl_insecure="1"`
	}
	return cmd
}
