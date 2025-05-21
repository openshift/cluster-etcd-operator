package pcs

import "testing"

const exampleStonithStatusJosn = `
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
	// Test successful unmarshaling
	config, err := UnmarshalStonithConfig(exampleStonithStatusJosn)
	if err != nil {
		t.Errorf("Failed to unmarshal valid JSON: %v", err)
	}

	// Verify expected primitive IDs
	expectedIDs := []string{"master-1_redfish", "master-0_redfish"}
	if len(config.Primitives) != len(expectedIDs) {
		t.Errorf("Expected %d primitives, got %d", len(expectedIDs), len(config.Primitives))
	}
	for i, id := range expectedIDs {
		if config.Primitives[i].Id != id {
			t.Errorf("Expected primitive ID %s, got %s", id, config.Primitives[i].Id)
		}
	}

	// Test error handling with invalid JSON
	_, err = UnmarshalStonithConfig("invalid json")
	if err == nil {
		t.Error("Expected error for invalid JSON, got nil")
	}
}
