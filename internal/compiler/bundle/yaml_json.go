package bundle

import (
	"encoding/json"

	"gopkg.in/yaml.v3"
)

func yamlUnmarshal(data []byte, out any) error { return yaml.Unmarshal(data, out) }

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
