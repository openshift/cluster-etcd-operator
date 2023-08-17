package hwspeedhelpers

import (
	"fmt"

	operatorv1 "github.com/openshift/api/operator/v1"
)

func HardwareSpeedToEnvMap(speed operatorv1.ControlPlaneHardwareSpeed) (envs map[string]string, err error) {
	switch speed {
	case operatorv1.StandardHardwareSpeed:
		envs = StandardHardwareSpeed()
	case operatorv1.SlowerHardwareSpeed:
		envs = SlowerHardwareSpeed()
	default:
		return nil, fmt.Errorf("invalid hardware speed value for etcd %v", speed)
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
