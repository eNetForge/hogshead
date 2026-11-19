package cmds

import "encoding/json"

// jsonRaw passes a pre-encoded JSON document through the client encoder.
type jsonRaw string

func (j jsonRaw) MarshalJSON() ([]byte, error) {
	if !json.Valid([]byte(j)) {
		return nil, &json.SyntaxError{}
	}
	return []byte(j), nil
}
