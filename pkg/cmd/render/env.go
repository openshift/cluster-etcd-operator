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
}

type envVarFunc func(platform, arch string) (map[string]string, error)

func getEtcdEnv(platform, arch string) (map[string]string, error) {
	ret := map[string]string{}
	for _, envVarFn := range envVarFns {
		newEnvVars, err := envVarFn(platform, arch)
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

func getFixedEtcdEnvVars(platform, arch string) (map[string]string, error) {
	env := etcdenvvar.FixedEtcdEnvVars
	// single member cluster needs to start with "new" (default).
	delete(env, "ETCD_INITIAL_CLUSTER_STATE")
	return env, nil
}

func getEtcdName(platform, arch string) (map[string]string, error) {
	return map[string]string{
		"ETCD_NAME": "etcd-bootstrap",
	}, nil
}

func getHeartbeatInterval(platform, arch string) (map[string]string, error) {
	var heartbeat string

	switch platform {
	case "Azure":
		heartbeat = "500"
	default:
		heartbeat = "100"
	}

	return map[string]string{
		"ETCD_HEARTBEAT_INTERVAL": heartbeat,
	}, nil
}

func getElectionTimeout(platform, arch string) (map[string]string, error) {
	var timeout string

	switch platform {
	case "Azure":
		timeout = "2500"
	default:
		timeout = "1000"
	}

	return map[string]string{
		"ETCD_ELECTION_TIMEOUT": timeout,
	}, nil

}

func getUnsupportedArch(platform, arch string) (map[string]string, error) {
	switch arch {
	case "arm64":
	case "s390x":
	default:
		// dont set unless it is defined.
		return nil, nil
	}
	return map[string]string{
		"ETCD_UNSUPPORTED_ARCH": arch,
	}, nil
}

// getTLSCipherSuites defines the ciphers used by the bootstrap etcd instance. The list is based on the definition of
// TLSProfileIntermediateType with a TLS version of 1.2.
func getTLSCipherSuites(platform, arch string) (map[string]string, error) {
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
