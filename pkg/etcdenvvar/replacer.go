package etcdenvvar

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

func GetSubstitutionReplacer(envVarMap map[string]string, imagePullSpec string) (*strings.Replacer, error) {
	if len(envVarMap) == 0 {
		return nil, fmt.Errorf("missing env var values")
	}

	envVarLines := []string{}
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		envVarLines = append(envVarLines, fmt.Sprintf("      - name: %q", k))
		envVarLines = append(envVarLines, fmt.Sprintf("        value: %q", v))
	}

	return strings.NewReplacer(
		"${IMAGE}", imagePullSpec,
		"${LISTEN_ON_ALL_IPS}", "0.0.0.0", // TODO this needs updating to detect ipv6-ness
		"${LOCALHOST_IP}", "127.0.0.1", // TODO this needs updating to detect ipv6-ness
		"${COMPUTED_ENV_VARS}", strings.Join(envVarLines, "\n"), // lacks beauty, but it works
	), nil
}
