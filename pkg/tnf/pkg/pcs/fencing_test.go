package pcs

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestGetFencingConfig(t *testing.T) {
	tests := []struct {
		name        string
		nodeName    string
		secret      *corev1.Secret
		wantErr     bool
		expectedCfg *fencingConfig
	}{
		{
			name:     "valid redfish config with ssl-insecure",
			nodeName: "node1",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"address":                 []byte("redfish+https://192.168.111.1:8000/redfish/v1/Systems/abc"),
					"username":                []byte("admin"),
					"password":                []byte("pass123"),
					"certificateVerification": []byte("Disabled"),
				},
			},
			wantErr: false,
			expectedCfg: &fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Ip:          "192.168.111.1",
					IpPort:      "8000",
					SystemsUri:  "/redfish/v1/Systems/abc",
					Username:    "admin",
					Password:    "pass123",
					SslInsecure: "",
				},
			},
		},
		{
			name:     "valid redfish config without ssl-insecure",
			nodeName: "node1",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"address":                 []byte("redfish+https://192.168.111.1:8000/redfish/v1/Systems/abc"),
					"username":                []byte("admin"),
					"password":                []byte("pass123"),
					"certificateVerification": []byte("Enabled"),
				},
			},
			wantErr: false,
			expectedCfg: &fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Ip:         "192.168.111.1",
					IpPort:     "8000",
					SystemsUri: "/redfish/v1/Systems/abc",
					Username:   "admin",
					Password:   "pass123",
				},
			},
		},
		{
			name:     "valid redfish config without port number on https",
			nodeName: "node1",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"address":                 []byte("redfish+https://192.168.111.1/redfish/v1/Systems/abc"),
					"username":                []byte("admin"),
					"password":                []byte("pass123"),
					"certificateVerification": []byte("Enabled"),
				},
			},
			wantErr: false,
			expectedCfg: &fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Ip:         "192.168.111.1",
					IpPort:     "443",
					SystemsUri: "/redfish/v1/Systems/abc",
					Username:   "admin",
					Password:   "pass123",
				},
			},
		},
		{
			name:     "valid redfish config without port number on http",
			nodeName: "node1",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"address":                 []byte("redfish+http://192.168.111.1/redfish/v1/Systems/abc"),
					"username":                []byte("admin"),
					"password":                []byte("pass123"),
					"certificateVerification": []byte("Enabled"),
				},
			},
			wantErr: false,
			expectedCfg: &fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Ip:         "192.168.111.1",
					IpPort:     "80",
					SystemsUri: "/redfish/v1/Systems/abc",
					Username:   "admin",
					Password:   "pass123",
				},
			},
		},
		{
			name:     "valid redfish config without port number on http ipv6",
			nodeName: "node1",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"address":                 []byte("redfish+http://[0000:0000:0000::0000]/redfish/v1/Systems/abc"),
					"username":                []byte("admin"),
					"password":                []byte("pass123"),
					"certificateVerification": []byte("Enabled"),
				},
			},
			wantErr: false,
			expectedCfg: &fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Ip:         "0000:0000:0000::0000",
					IpPort:     "80",
					SystemsUri: "/redfish/v1/Systems/abc",
					Username:   "admin",
					Password:   "pass123",
				},
			},
		},
		{
			name:     "invalid address format",
			nodeName: "node1",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"address":                 []byte("invalid-address"),
					"username":                []byte("admin"),
					"password":                []byte("pass123"),
					"certificateVerification": []byte("Disabled"),
				},
			},
			wantErr: true,
		},
		{
			name:     "missing username",
			nodeName: "node1",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"address":                 []byte("redfish+https://192.168.111.1:8000/redfish/v1/Systems/abc"),
					"password":                []byte("pass123"),
					"certificateVerification": []byte("Disabled"),
				},
			},
			wantErr: true,
		},
		{
			name:     "missing password",
			nodeName: "node1",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"address":                 []byte("redfish+https://192.168.111.1:8000/redfish/v1/Systems/abc"),
					"username":                []byte("admin"),
					"certificateVerification": []byte("Disabled"),
				},
			},
			wantErr: true,
		},
		{
			name:     "missing port and no schema provided",
			nodeName: "node1",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"address":                 []byte("192.168.111.1/redfish/v1/Systems/abc"),
					"username":                []byte("admin"),
					"certificateVerification": []byte("Disabled"),
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := getFencingConfig(tt.nodeName, tt.secret)
			if (err != nil) != tt.wantErr {
				t.Errorf("getFencingConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !compareFencingConfigs(cfg, tt.expectedCfg) {
				t.Errorf("getFencingConfig() = %v, want %v", cfg, tt.expectedCfg)
			}
		})
	}
}

func TestGetStatusCommand(t *testing.T) {
	tests := []struct {
		name string
		fc   fencingConfig
		want string
	}{
		{
			name: "status command without ssl-insecure",
			fc: fencingConfig{
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:   "admin",
					Password:   "pass123",
					Ip:         "192.168.111.1",
					IpPort:     "8000",
					SystemsUri: "/redfish/v1/Systems/abc",
				},
			},
			want: "/usr/sbin/fence_redfish --username admin --password pass123 --ip 192.168.111.1 --ipport 8000 --systems-uri /redfish/v1/Systems/abc --action status",
		},
		{
			name: "status command with ssl-insecure",
			fc: fencingConfig{
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:    "admin",
					Password:    "pass123",
					Ip:          "192.168.111.1",
					IpPort:      "8000",
					SystemsUri:  "/redfish/v1/Systems/abc",
					SslInsecure: "",
				},
			},
			want: "/usr/sbin/fence_redfish --username admin --password pass123 --ip 192.168.111.1 --ipport 8000 --systems-uri /redfish/v1/Systems/abc --action status --ssl-insecure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getStatusCommand(tt.fc); got != tt.want {
				t.Errorf("getStatusCommand() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetStonithCommand(t *testing.T) {
	tests := []struct {
		name string
		sc   StonithConfig
		fc   fencingConfig
		want string
	}{
		{
			name: "stonith command without ssl_insecure",
			fc: fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:   "admin",
					Password:   "pass123",
					Ip:         "192.168.111.1",
					IpPort:     "8000",
					SystemsUri: "redfish/v1/Systems/abc",
				},
			},
			want: `/usr/sbin/pcs stonith create node1_redfish fence_redfish username="admin" password="pass123" ip="192.168.111.1" ipport="8000" systems_uri="redfish/v1/Systems/abc" pcmk_host_list="node1" pcmk_delay_base="" ssl_insecure="0" --wait=120`,
		},
		{
			name: "stonith command with ssl_insecure",
			fc: fencingConfig{
				NodeName:          "node2",
				FencingID:         "node2_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:    "admin",
					Password:    "pass123",
					Ip:          "192.168.111.2",
					IpPort:      "8000",
					SystemsUri:  "redfish/v1/Systems/def",
					SslInsecure: "",
				},
			},
			want: `/usr/sbin/pcs stonith create node2_redfish fence_redfish username="admin" password="pass123" ip="192.168.111.2" ipport="8000" systems_uri="redfish/v1/Systems/def" pcmk_host_list="node2" pcmk_delay_base="" ssl_insecure="1" --wait=120`,
		},
		{
			name: "stonith command with ipv6",
			fc: fencingConfig{
				NodeName:          "node2",
				FencingID:         "node2_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:    "admin",
					Password:    "pass123",
					Ip:          "1234:1234:1234::1234",
					IpPort:      "8000",
					SystemsUri:  "redfish/v1/Systems/def",
					SslInsecure: "",
				},
			},
			want: `/usr/sbin/pcs stonith create node2_redfish fence_redfish username="admin" password="pass123" ip="[1234:1234:1234::1234]" ipport="8000" systems_uri="redfish/v1/Systems/def" pcmk_host_list="node2" pcmk_delay_base="" ssl_insecure="1" --wait=120`,
		},
		{
			name: "stonith command with pcmk_delay_base",
			fc: fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:      "admin",
					Password:      "pass123",
					Ip:            "192.168.111.1",
					IpPort:        "8000",
					SystemsUri:    "redfish/v1/Systems/abc",
					PcmkDelayBase: "10s",
				},
			},
			want: `/usr/sbin/pcs stonith create node1_redfish fence_redfish username="admin" password="pass123" ip="192.168.111.1" ipport="8000" systems_uri="redfish/v1/Systems/abc" pcmk_host_list="node1" pcmk_delay_base="10s" ssl_insecure="0" --wait=120`,
		},
		{
			name: "stonith command with both ssl_insecure and pcmk_delay_base",
			fc: fencingConfig{
				NodeName:          "node3",
				FencingID:         "node3_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:      "admin",
					Password:      "pass123",
					Ip:            "192.168.111.3",
					IpPort:        "8000",
					SystemsUri:    "redfish/v1/Systems/ghi",
					SslInsecure:   "",
					PcmkDelayBase: "10s",
				},
			},
			want: `/usr/sbin/pcs stonith create node3_redfish fence_redfish username="admin" password="pass123" ip="192.168.111.3" ipport="8000" systems_uri="redfish/v1/Systems/ghi" pcmk_host_list="node3" pcmk_delay_base="10s" ssl_insecure="1" --wait=120`,
		},
		{
			name: "stonith command with existing device",
			sc: StonithConfig{
				Primitives: []Primitive{
					{Id: "node2_redfish"},
				},
			},
			fc: fencingConfig{
				NodeName:          "node2",
				FencingID:         "node2_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:    "admin",
					Password:    "pass123",
					Ip:          "192.168.111.2",
					IpPort:      "8000",
					SystemsUri:  "redfish/v1/Systems/def",
					SslInsecure: "",
				},
			},
			want: `/usr/sbin/pcs stonith update node2_redfish fence_redfish username="admin" password="pass123" ip="192.168.111.2" ipport="8000" systems_uri="redfish/v1/Systems/def" pcmk_host_list="node2" pcmk_delay_base="" ssl_insecure="1" --wait=120`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getStonithCommand(tt.sc, tt.fc)
			if got != tt.want {
				t.Errorf("getStonithCommand() =\n%v\nwant:\n%v", got, tt.want)
			}
		})
	}
}

// Helper function to compare fencing configs
func compareFencingConfigs(a, b *fencingConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.NodeName != b.NodeName ||
		a.FencingID != b.FencingID ||
		a.FencingDeviceType != b.FencingDeviceType {
		return false
	}
	if len(a.FencingDeviceOptions) != len(b.FencingDeviceOptions) {
		return false
	}
	for k, v := range a.FencingDeviceOptions {
		if bv, ok := b.FencingDeviceOptions[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func TestCanFencingConfigBeSkipped(t *testing.T) {
	tests := []struct {
		name string
		fc   fencingConfig
		sc   StonithConfig
		want bool
	}{
		{
			name: "needs update for wrong ip port",
			fc: fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:    "admin",
					Password:    "pass123",
					Ip:          "192.168.111.1",
					IpPort:      "8000",
					SystemsUri:  "redfish/v1/Systems/abc",
					SslInsecure: "does not matter",
				},
			},
			sc: StonithConfig{
				Primitives: []Primitive{
					{
						Id: "node1_redfish",
						AgentName: AgentName{
							Type: "fence_redfish",
						},
						InstanceAttributes: []InstanceAttributes{
							{
								NvPairs: []NvPair{
									{
										Name:  "username",
										Value: "admin",
									},
									{
										Name:  "password",
										Value: "pass123",
									},
									{
										Name:  "ip",
										Value: "192.168.111.1",
									},
									{
										Name:  "ipport",
										Value: "4242",
									},
									{
										Name:  "systems_uri",
										Value: "redfish/v1/Systems/abc",
									},
									{
										Name:  "pcmk_host_list",
										Value: "node1",
									},
									{
										Name:  "ssl_insecure",
										Value: "1",
									},
								},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "needs update for missing ssl insecure",
			fc: fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:    "admin",
					Password:    "pass123",
					Ip:          "192.168.111.1",
					IpPort:      "8000",
					SystemsUri:  "redfish/v1/Systems/abc",
					SslInsecure: "does not matter",
				},
			},
			sc: StonithConfig{
				Primitives: []Primitive{
					{
						Id: "node1_redfish",
						AgentName: AgentName{
							Type: "fence_redfish",
						},
						InstanceAttributes: []InstanceAttributes{
							{
								NvPairs: []NvPair{
									{
										Name:  "username",
										Value: "admin",
									},
									{
										Name:  "password",
										Value: "pass123",
									},
									{
										Name:  "ip",
										Value: "192.168.111.1",
									},
									{
										Name:  "ipport",
										Value: "8000",
									},
									{
										Name:  "systems_uri",
										Value: "redfish/v1/Systems/abc",
									},
									{
										Name:  "pcmk_host_list",
										Value: "node1",
									},
								},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "needs update for agent type",
			fc: fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:   "admin",
					Password:   "pass123",
					Ip:         "192.168.111.1",
					IpPort:     "8000",
					SystemsUri: "redfish/v1/Systems/abc",
				},
			},
			sc: StonithConfig{
				Primitives: []Primitive{
					{
						Id: "node1_redfish",
						AgentName: AgentName{
							Type: "fence_bar",
						},
						InstanceAttributes: []InstanceAttributes{
							{
								NvPairs: []NvPair{
									{
										Name:  "username",
										Value: "admin",
									},
									{
										Name:  "password",
										Value: "pass123",
									},
									{
										Name:  "ip",
										Value: "192.168.111.1",
									},
									{
										Name:  "ipport",
										Value: "8000",
									},
									{
										Name:  "systems_uri",
										Value: "redfish/v1/Systems/abc",
									},
									{
										Name:  "pcmk_host_list",
										Value: "node1",
									},
								},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "can be skipped",
			fc: fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:   "admin",
					Password:   "pass123",
					Ip:         "192.168.111.1",
					IpPort:     "8000",
					SystemsUri: "redfish/v1/Systems/abc",
				},
			},
			sc: StonithConfig{
				Primitives: []Primitive{
					{
						Id: "node1_redfish",
						AgentName: AgentName{
							Type: "fence_redfish",
						},
						InstanceAttributes: []InstanceAttributes{
							{
								NvPairs: []NvPair{
									{
										Name:  "username",
										Value: "admin",
									},
									{
										Name:  "password",
										Value: "pass123",
									},
									{
										Name:  "ip",
										Value: "192.168.111.1",
									},
									{
										Name:  "ipport",
										Value: "8000",
									},
									{
										Name:  "systems_uri",
										Value: "redfish/v1/Systems/abc",
									},
									{
										Name:  "pcmk_host_list",
										Value: "node1",
									},
								},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "needs update for missing pcmk_delay_base",
			fc: fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:      "admin",
					Password:      "pass123",
					Ip:            "192.168.111.1",
					IpPort:        "8000",
					SystemsUri:    "redfish/v1/Systems/abc",
					PcmkDelayBase: "10s",
				},
			},
			sc: StonithConfig{
				Primitives: []Primitive{
					{
						Id: "node1_redfish",
						AgentName: AgentName{
							Type: "fence_redfish",
						},
						InstanceAttributes: []InstanceAttributes{
							{
								NvPairs: []NvPair{
									{
										Name:  "username",
										Value: "admin",
									},
									{
										Name:  "password",
										Value: "pass123",
									},
									{
										Name:  "ip",
										Value: "192.168.111.1",
									},
									{
										Name:  "ipport",
										Value: "8000",
									},
									{
										Name:  "systems_uri",
										Value: "redfish/v1/Systems/abc",
									},
									{
										Name:  "pcmk_host_list",
										Value: "node1",
									},
								},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "can be skipped with pcmk_delay_base",
			fc: fencingConfig{
				NodeName:          "node1",
				FencingID:         "node1_redfish",
				FencingDeviceType: "fence_redfish",
				FencingDeviceOptions: map[fencingOption]string{
					Username:      "admin",
					Password:      "pass123",
					Ip:            "192.168.111.1",
					IpPort:        "8000",
					SystemsUri:    "redfish/v1/Systems/abc",
					PcmkDelayBase: "10s",
				},
			},
			sc: StonithConfig{
				Primitives: []Primitive{
					{
						Id: "node1_redfish",
						AgentName: AgentName{
							Type: "fence_redfish",
						},
						InstanceAttributes: []InstanceAttributes{
							{
								NvPairs: []NvPair{
									{
										Name:  "username",
										Value: "admin",
									},
									{
										Name:  "password",
										Value: "pass123",
									},
									{
										Name:  "ip",
										Value: "192.168.111.1",
									},
									{
										Name:  "ipport",
										Value: "8000",
									},
									{
										Name:  "systems_uri",
										Value: "redfish/v1/Systems/abc",
									},
									{
										Name:  "pcmk_host_list",
										Value: "node1",
									},
									{
										Name:  "pcmk_delay_base",
										Value: "10s",
									},
								},
							},
						},
					},
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canFencingConfigBeSkipped(tt.fc, tt.sc); got != tt.want {
				t.Errorf("needsUpdate() = %v, want %v", got, tt.want)
			}
		})
	}

}
