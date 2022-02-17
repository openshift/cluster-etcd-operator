package render

import (
	"encoding/json"
	"testing"

	"github.com/ghodss/yaml"
)

var (
	installConfigSNO = `
    apiVersion: v1
    baseDomain: test.com
    compute:
    - architecture: amd64
      name: worker
      replicas: 3
    controlPlane:
      name: master
      replicas: 1
`
	installConfigHA = `
    apiVersion: v1
    baseDomain: test.com
    compute:
    - architecture: amd64
      name: worker
      replicas: 3
    controlPlane:
      name: master
      replicas: 3
`
)

func TestEtcdEnv(t *testing.T) {
	tests := []struct {
		name          string
		platform      string
		arch          string
		wantKey       string
		wantValue     string
		installConfig string
		platformData  string
	}{
		{
			name:          "AWS HA etcd heartbeat interval",
			platform:      "AWS",
			arch:          "amd64",
			wantKey:       "ETCD_HEARTBEAT_INTERVAL",
			installConfig: installConfigHA,
			wantValue:     "100",
		},
		{
			name:          "None SNO etcd election timeout",
			platform:      "None",
			arch:          "ppc64le",
			wantKey:       "ETCD_HEARTBEAT_INTERVAL",
			installConfig: installConfigSNO,
			wantValue:     "100",
		},
		{
			name:          "Azure HA etcd heartbeat interval",
			platform:      "Azure",
			arch:          "amd64",
			wantKey:       "ETCD_HEARTBEAT_INTERVAL",
			installConfig: installConfigHA,
			wantValue:     "500",
		},
		{
			name:          "Azure HA etcd election timeout",
			platform:      "Azure",
			arch:          "amd64",
			wantKey:       "ETCD_ELECTION_TIMEOUT",
			installConfig: installConfigHA,
			wantValue:     "2500",
		},
		{
			name:          "None HA s390x",
			platform:      "None",
			arch:          "s390x",
			wantKey:       "ETCD_UNSUPPORTED_ARCH",
			installConfig: installConfigHA,
			wantValue:     "s390x",
		},
		{
			name:          "None SNO arm64",
			platform:      "None",
			arch:          "arm64",
			wantKey:       "ETCD_UNSUPPORTED_ARCH",
			installConfig: installConfigSNO,
			wantValue:     "arm64",
		},
		{
			name:          "GCP SNO max learners",
			platform:      "GCP",
			arch:          "arm64",
			wantKey:       "ETCD_EXPERIMENTAL_MAX_LEARNERS",
			installConfig: installConfigSNO,
			wantValue:     "1",
		},
		{
			name:          "None HA max learners",
			platform:      "None",
			arch:          "arm64",
			wantKey:       "ETCD_EXPERIMENTAL_MAX_LEARNERS",
			installConfig: installConfigHA,
			wantValue:     "3",
		},
		{
			name:          "IBMCloud VPC etcd election timeout",
			platform:      "IBMCloud",
			platformData:  "VPC",
			arch:          "amd64",
			wantKey:       "ETCD_ELECTION_TIMEOUT",
			installConfig: installConfigHA,
			wantValue:     "2000",
		},
		{
			name:          "IBMCloud non-VPC etcd election timeout",
			platform:      "IBMCloud",
			platformData:  "Classic",
			arch:          "amd64",
			wantKey:       "ETCD_ELECTION_TIMEOUT",
			installConfig: installConfigHA,
			wantValue:     "1000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var installConfig map[string]interface{}
			installConfigJson, _ := yaml.YAMLToJSON([]byte(tt.installConfig))
			err := json.Unmarshal(installConfigJson, &installConfig)
			if err != nil {
				t.Errorf("failed to unmarshal install config json %v", err)
			}
			data := &envVarData{platform: tt.platform, platformData: tt.platformData, arch: tt.arch, installConfig: installConfig}
			env, err := getEtcdEnv(data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if env[tt.wantKey] != tt.wantValue {
				t.Errorf("getEtcdEnv(%q, %q) =  want %q = %q got %q = %q", tt.platform, tt.arch, tt.wantKey, tt.wantValue, tt.wantKey, env[tt.wantKey])
			}
		})
	}
}
