package tools

import "encoding/json"

func AddToRawJson(in []byte, key string, value any) ([]byte, error) {

	var jsonData map[string]interface{}

	if in == nil || len(in) == 0 {
		in = []byte(`{}`)
	}

	err := json.Unmarshal(in, &jsonData)
	if err != nil {
		return nil, err
	}

	jsonData[key] = value
	out, err := json.Marshal(jsonData)
	if err != nil {
		return nil, err
	}

	return out, nil

}
