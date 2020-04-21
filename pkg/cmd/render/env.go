package render

import (
	"fmt"
	"strings"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
)

var envVarFns = []envVarFunc{
	getFixedEtcdEnvVars,
	getUnsupportedArch,
	getEtcdName,
}

type envVarFunc func(arch string) (map[string]string, error)

func getEtcdEnv(arch string) (map[string]string, error) {
	ret := map[string]string{}
	for _, envVarFn := range envVarFns {
		newEnvVars, err := envVarFn(arch)
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

func getFixedEtcdEnvVars(arch string) (map[string]string, error) {
	env := etcdenvvar.FixedEtcdEnvVars
	// single member cluster needs to start with "new" (default).
	delete(env, "ETCD_INITIAL_CLUSTER_STATE")
	return env, nil
}

func getEtcdName(arch string) (map[string]string, error) {
	return map[string]string{
		"ETCD_NAME": "etcd-bootstrap",
	}, nil
}

func getUnsupportedArch(arch string) (map[string]string, error) {
	if !strings.HasPrefix(arch, "s390") {
		// dont set unless it is defined.
		return nil, nil
	}
	return map[string]string{
		"ETCD_UNSUPPORTED_ARCH": arch,
	}, nil
}
