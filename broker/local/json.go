package local

import "encoding/json"

func decodeJSON(data []byte, target any) error { return json.Unmarshal(data, target) }
func encodeJSON(value any) ([]byte, error)     { return json.Marshal(value) }
