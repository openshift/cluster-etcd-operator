package render

import (
	"fmt"
	"strings"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
)

var envVarFns = []envVarFunc{
	getFixedEtcdEnvVars,
	getHeartbeatInterval,
	getElectionTimeout,
	getUnsupportedArch,
	getEtcdName,
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
	if !strings.HasPrefix(arch, "s390") {
		// dont set unless it is defined.
		return nil, nil
	}
	return map[string]string{
		"ETCD_UNSUPPORTED_ARCH": arch,
	}, nil
}
