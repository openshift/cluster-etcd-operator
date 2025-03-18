package config

import "testing"

func TestClusterConfig(t *testing.T) {

	yaml := `
master-0:
  address: redfish+http://192.168.111.1:8000/redfish/v1/Systems/ba600ee3-5fa7-4ece-b424-7cd0d3355fea
  username: admin
  password: password
  sslInsecure: true
master-1:
  address: redfish+http://192.168.111.1:8000/redfish/v1/Systems/0346d757-a037-4c7f-9c61-2294357af8f4
  username: admin
  password: password
  sslInsecure: true
`

	fcs, err := parseConfigYaml(yaml)
	if err != nil {
		t.Errorf("Failed to parse yaml: %v", err)
	}
	if len(fcs) != 2 {
		t.Errorf("Wrong number of FencingConfigs parsed from yaml")
	}
}
