package pcs

import "encoding/json"

// StonithConfig represents the minimum part of the configuration of stonith devices.
type StonithConfig struct {
	Primitives []struct {
		Id string `json:"id"`
	} `json:"primitives"`
}

func UnmarshalStonithConfig(data string) (StonithConfig, error) {
	var stonithConfig StonithConfig
	err := json.Unmarshal([]byte(data), &stonithConfig)
	if err != nil {
		return stonithConfig, err
	}
	return stonithConfig, nil
}
