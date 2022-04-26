package render

import (
	"fmt"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	"github.com/openshift/library-go/pkg/crypto"
)

var envVarFns = []envVarFunc{
	getFixedEtcdEnvVars,
	getHeartbeatInterval,
	getElectionTimeout,
	getUnsupportedArch,
	getEtcdName,
	getTLSCipherSuites,
	getMaxLearners,
}

type envVarData struct {
	platform      string
	arch          string
	installConfig map[string]interface{}
	platformData  string
}

type envVarFunc func(e *envVarData) (map[string]string, error)

func getEtcdEnv(e *envVarData) (map[string]string, error) {
	ret := map[string]string{}
	for _, envVarFn := range envVarFns {
		newEnvVars, err := envVarFn(e)
		if err != nil {
			return nil, err
		}
		if newEnvVars == nil {
			continue
		}
		for k, v := range newEnvVars {
			if currV, ok := ret[k]; ok {
				return nil, fmt.Errorf("key %q already set to %q", k, currV)
			}
			ret[k] = v
		}
	}
	return ret, nil
}

func getFixedEtcdEnvVars(_ *envVarData) (map[string]string, error) {
	env := etcdenvvar.FixedEtcdEnvVars
	// single member cluster needs to start with "new" (default).
	delete(env, "ETCD_INITIAL_CLUSTER_STATE")
	return env, nil
}

func getEtcdName(_ *envVarData) (map[string]string, error) {
	return map[string]string{
		"ETCD_NAME": "etcd-bootstrap",
	}, nil
}

func getHeartbeatInterval(e *envVarData) (map[string]string, error) {
	heartbeat := "100"

	switch configv1.PlatformType(e.platform) {
	case configv1.AzurePlatformType:
		heartbeat = "500"
	case configv1.IBMCloudPlatformType:
		switch configv1.IBMCloudProviderType(e.platformData) {
		case configv1.IBMCloudProviderTypeVPC:
			heartbeat = "500"
		}
	}

	return map[string]string{
		"ETCD_HEARTBEAT_INTERVAL": heartbeat,
	}, nil
}

func getElectionTimeout(e *envVarData) (map[string]string, error) {
	timeout := "1000"

	switch configv1.PlatformType(e.platform) {
	case configv1.AzurePlatformType:
		timeout = "2500"
	case configv1.IBMCloudPlatformType:
		switch configv1.IBMCloudProviderType(e.platformData) {
		case configv1.IBMCloudProviderTypeVPC:
			timeout = "2500"
		}
	}

	return map[string]string{
		"ETCD_ELECTION_TIMEOUT": timeout,
	}, nil

}

func getUnsupportedArch(e *envVarData) (map[string]string, error) {
	switch e.arch {
	case "arm64":
	case "s390x":
	default:
		// dont set unless it is defined.
		return nil, nil
	}
	return map[string]string{
		"ETCD_UNSUPPORTED_ARCH": e.arch,
	}, nil
}

// getTLSCipherSuites defines the ciphers used by the bootstrap etcd instance. The list is based on the definition of
// TLSProfileIntermediateType with a TLS version of 1.2.
func getTLSCipherSuites(_ *envVarData) (map[string]string, error) {
	profileSpec := configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
	cipherSuites := crypto.OpenSSLToIANACipherSuites(profileSpec.Ciphers)
	if len(cipherSuites) == 0 {
		return nil, fmt.Errorf("no valid TLS ciphers found")
	}
	// Remove invalid ciphers.
	cipherSuites = tlshelpers.SupportedEtcdCiphers(cipherSuites)
	return map[string]string{
		"ETCD_CIPHER_SUITES": strings.Join(cipherSuites, ","),
	}, nil
}

func getMaxLearners(e *envVarData) (map[string]string, error) {
	controlPlane, found := e.installConfig["controlPlane"].(map[string]interface{})
	if !found {
		return nil, fmt.Errorf("unrecognized data structure in controlPlane field")
	}
	replicaCount, found := controlPlane["replicas"].(float64)
	if !found {
		return nil, fmt.Errorf("unrecognized data structure in controlPlane replica field")
	}
	return map[string]string{
		"ETCD_EXPERIMENTAL_MAX_LEARNERS": fmt.Sprint(replicaCount),
	}, nil
}
