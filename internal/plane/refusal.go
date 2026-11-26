package plane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// refuseWithPlaneWords renders a rejected asset request from the plane's own
// words. Its authorization failures are JSON (`{"error":{"code","message"}}`)
// and its not-found is plain text; both must reach the caller verbatim, because
// only the plane knows which it meant. operation names the request the caller
// made ("download refused", "upload PUT refused"), so the same rendering cannot
// drift between the two asset routes.
func refuseWithPlaneWords(operation string, status int, raw []byte) error {
	var wire struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &wire); err == nil && wire.Error.Message != "" {
		if code := strings.TrimSpace(wire.Error.Code); code != "" {
			return fmt.Errorf("%s: HTTP %d %s: %s", operation, status, code, wire.Error.Message)
		}
		return fmt.Errorf("%s: HTTP %d: %s", operation, status, wire.Error.Message)
	}
	return fmt.Errorf("%s: HTTP %d: %s", operation, status, strings.TrimSpace(string(raw)))
}
