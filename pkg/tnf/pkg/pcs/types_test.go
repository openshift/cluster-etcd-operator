package pcs

import "testing"

const exampleStonithStatusJson = `
{
  "primitives": [
    {
      "id": "master-1_redfish",
      "agent_name": {
        "standard": "stonith",
        "provider": null,
        "type": "fence_redfish"
      },
      "description": null,
      "operations": [
        {
          "id": "master-1_redfish-monitor-interval-60s",
          "name": "monitor",
          "interval": "60s",
          "description": null,
          "start_delay": null,
          "interval_origin": null,
          "timeout": null,
          "enabled": null,
          "record_pending": null,
          "role": null,
          "on_fail": null,
          "meta_attributes": [],
          "instance_attributes": []
        }
      ],
      "meta_attributes": [],
      "instance_attributes": [
        {
          "id": "master-1_redfish-instance_attributes",
          "options": {},
          "rule": null,
          "nvpairs": [
            {
              "id": "master-1_redfish-instance_attributes-ip",
              "name": "ip",
              "value": "192.168.111.1"
            },
            {
              "id": "master-1_redfish-instance_attributes-ipport",
              "name": "ipport",
              "value": "8000"
            },
            {
              "id": "master-1_redfish-instance_attributes-password",
              "name": "password",
              "value": "password"
            },
            {
              "id": "master-1_redfish-instance_attributes-pcmk_host_list",
              "name": "pcmk_host_list",
              "value": "master-1"
            },
            {
              "id": "master-1_redfish-instance_attributes-ssl_insecure",
              "name": "ssl_insecure",
              "value": "1"
            },
            {
              "id": "master-1_redfish-instance_attributes-systems_uri",
              "name": "systems_uri",
              "value": "redfish/v1/Systems/ca7256ff-53bd-4216-bf41-e6baba3aa260"
            },
            {
              "id": "master-1_redfish-instance_attributes-username",
              "name": "username",
              "value": "admin"
            }
          ]
        }
      ],
      "utilization": []
    },
    {
      "id": "master-0_redfish",
      "agent_name": {
        "standard": "stonith",
        "provider": null,
        "type": "fence_redfish"
      },
      "description": null,
      "operations": [
        {
          "id": "master-0_redfish-monitor-interval-60s",
          "name": "monitor",
          "interval": "60s",
          "description": null,
          "start_delay": null,
          "interval_origin": null,
          "timeout": null,
          "enabled": null,
          "record_pending": null,
          "role": null,
          "on_fail": null,
          "meta_attributes": [],
          "instance_attributes": []
        }
      ],
      "meta_attributes": [],
      "instance_attributes": [
        {
          "id": "master-0_redfish-instance_attributes",
          "options": {},
          "rule": null,
          "nvpairs": [
            {
              "id": "master-0_redfish-instance_attributes-ip",
              "name": "ip",
              "value": "192.168.111.1"
            },
            {
              "id": "master-0_redfish-instance_attributes-ipport",
              "name": "ipport",
              "value": "8000"
            },
            {
              "id": "master-0_redfish-instance_attributes-password",
              "name": "password",
              "value": "password"
            },
            {
              "id": "master-0_redfish-instance_attributes-pcmk_host_list",
              "name": "pcmk_host_list",
              "value": "master-0"
            },
            {
              "id": "master-0_redfish-instance_attributes-ssl_insecure",
              "name": "ssl_insecure",
              "value": "1"
            },
            {
              "id": "master-0_redfish-instance_attributes-systems_uri",
              "name": "systems_uri",
              "value": "/redfish/v1/Systems/af2167e4-c13b-4941-b606-f912e9a86f4b"
            },
            {
              "id": "master-0_redfish-instance_attributes-username",
              "name": "username",
              "value": "admin"
            }
          ]
        }
      ],
      "utilization": []
    }
  ],
  "clones": [],
  "groups": [],
  "bundles": []
}
`

func TestUnmarshalStonithConfig(t *testing.T) {
	tests := []struct {
		name           string
		json           string
		expectedValues []struct {
			primitiveId string
			agentType   string
			nvPairs     []struct {
				Name  string
				Value string
			}
		}
		wantErr bool
	}{
		{
			name:           "empty json",
			json:           "{}",
			expectedValues: nil,
			wantErr:        false,
		},
		{
			name:           "invalid json",
			json:           "foo",
			expectedValues: nil,
			wantErr:        true,
		},
		{
			name: "valid json",
			json: exampleStonithStatusJson,
			expectedValues: []struct {
				primitiveId string
				agentType   string
				nvPairs     []struct {
					Name  string
					Value string
				}
			}{
				{
					primitiveId: "master-0_redfish",
					agentType:   "fence_redfish",
					nvPairs: []struct {
						Name  string
						Value string
					}{
						{
							Name:  "ip",
							Value: "192.168.111.1",
						},
						{
							Name:  "systems_uri",
							Value: "/redfish/v1/Systems/af2167e4-c13b-4941-b606-f912e9a86f4b",
						},
					},
				},
				{
					primitiveId: "master-1_redfish",
					agentType:   "fence_redfish",
					nvPairs: []struct {
						Name  string
						Value string
					}{
						{
							Name:  "pcmk_host_list",
							Value: "master-1",
						},
						{
							Name:  "ssl_insecure",
							Value: "1",
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := unmarshalStonithConfig(tt.json)
			if (err != nil) != tt.wantErr {
				t.Errorf("getFencingConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(config.Primitives) != len(tt.expectedValues) {
				t.Errorf("Expected %d primitives, got %v", len(tt.expectedValues), config)
			}
			for _, expected := range tt.expectedValues {
				found := false
				for _, p := range config.Primitives {
					if p.Id == expected.primitiveId {
						if p.AgentName.Type != expected.agentType {
							t.Errorf("Expected agent type %s, got %s", expected.agentType, p.AgentName.Type)
						}
						for _, expectedNv := range expected.nvPairs {
							for _, nv := range p.InstanceAttributes[0].NvPairs {
								if nv.Name == expectedNv.Name && nv.Value != expectedNv.Value {
									t.Errorf("Expected %s to be %s, got %s", nv.Name, expectedNv.Value, nv.Value)
								}
							}
						}
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected to find %v in %v", expected, config.Primitives)
				}
			}
		})
	}
}
