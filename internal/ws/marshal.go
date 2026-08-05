package ws

import "encoding/json"

func marshal(e Event) ([]byte, error) {
	return json.Marshal(e)
}
