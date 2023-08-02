package profilehelpers

import (
	"fmt"

	operatorv1 "github.com/openshift/api/operator/v1"
)

func ProfileToEnvMap(profile operatorv1.ControlPlaneHardwareSpeed) (envs map[string]string, err error) {
	switch profile {
	case operatorv1.StandardHardwareSpeed:
		envs = StandardHardwareSpeed()
	case operatorv1.SlowerHardwareSpeed:
		envs = SlowerHardwareSpeed()
	default:
		return nil, fmt.Errorf("invalid etcd tuning profile %v", profile)
	}
	return envs, nil
}

func StandardHardwareSpeed() map[string]string {
	return map[string]string{
		"ETCD_HEARTBEAT_INTERVAL": "100",
		"ETCD_ELECTION_TIMEOUT":   "1000",
	}
}

func SlowerHardwareSpeed() map[string]string {
	return map[string]string{
		"ETCD_HEARTBEAT_INTERVAL": "500",
		"ETCD_ELECTION_TIMEOUT":   "2500",
	}
}
