package render

import "testing"

func TestEtcdEnv(t *testing.T) {
	for _, test := range []struct {
		arch, wantKey, wantValue string
	}{
		{"s390x", "ETCD_UNSUPPORTED_ARCH", "s390x"},
	} {
		env, _ := getEtcdEnv(test.arch)
		if env[test.wantKey] != test.wantValue {
			t.Errorf("getEtcdEnv(%q) =  want %q = %q got %q = %q", test.arch, test.wantKey, test.wantValue, test.wantKey, env[test.wantKey])
		}
	}
}
