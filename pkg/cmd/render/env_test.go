package render

import "testing"

func TestEtcdEnv(t *testing.T) {
	for _, test := range []struct {
		platform, arch, wantKey, wantValue string
	}{
		{"AWS", "amd64", "ETCD_HEARTBEAT_INTERVAL", "100"},
		{"AWS", "amd64", "ETCD_ELECTION_TIMEOUT", "1000"},
		{"Azure", "ppc64le", "ETCD_HEARTBEAT_INTERVAL", "500"},
		{"Azure", "amd64", "ETCD_ELECTION_TIMEOUT", "2500"},
		{"None", "ppc64le", "ETCD_HEARTBEAT_INTERVAL", "100"},
		{"Azure", "s390x", "ETCD_UNSUPPORTED_ARCH", "s390x"},
		{"None", "arm64", "ETCD_UNSUPPORTED_ARCH", "arm64"},
	} {
		env, _ := getEtcdEnv(test.platform, test.arch)
		if env[test.wantKey] != test.wantValue {
			t.Errorf("getEtcdEnv(%q, %q) =  want %q = %q got %q = %q", test.platform, test.arch, test.wantKey, test.wantValue, test.wantKey, env[test.wantKey])
		}
	}
}
