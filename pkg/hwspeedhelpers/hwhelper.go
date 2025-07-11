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

// Note(everettraven): OCPBUGS-50510: From what I could gleam, on our
// "standard" hardware platforms for CI we seem to have an average RTT of ~12ms
// between etcd peers. Looking at the telemetry I was able to gather, this
// is about half of what the average RTT on all clusters reporting
// telemetry tends to be (~25ms).
// Assuming clusters out in the wild set to "standard" hardware speed
// are roughly equivalent to our CI environments, our heartbeat interval on these platforms should
// be, at the top end, ~20ms. The election timeout should be 10x that.
// With the change in heartbeat interval and leader election,
// we should also appropriately adjust the static timeout
// delay that was added in https://github.com/openshift/etcd/pull/311
func StandardHardwareSpeed() map[string]string {
	return map[string]string{
        // in milliseconds
		"ETCD_HEARTBEAT_INTERVAL": "20",
		"ETCD_ELECTION_TIMEOUT":   "200",
        // in seconds
        "OPENSHIFT_ETCD_HARDWARE_DELAY_TIMEOUT": "30",
	}
}


// Note(everettraven): OCPBUGS-50510: From what I could gleam, on our
// "slower" hardware platforms for CI we seem to have an average RTT of ~25ms
// between etcd peers. Looking at the telemetry I was able to gather, this
// is pretty much in line with th RTT on all clusters reporting
// telemetry tends to be (~25ms).
// This means our heartbeat interval on these platforms should
// be, at the top end, ~35ms. The election timeout should be 10x that.
// With the change in heartbeat interval and leader election,
// we should also appropriately adjust the static timeout
// delay that was added in https://github.com/openshift/etcd/pull/311
func SlowerHardwareSpeed() map[string]string {
	return map[string]string{
        // in milliseconds
		"ETCD_HEARTBEAT_INTERVAL": "35",
		"ETCD_ELECTION_TIMEOUT":   "350",
        // in seconds
        "OPENSHIFT_ETCD_HARDWARE_DELAY_TIMEOUT": "30",
	}
}
