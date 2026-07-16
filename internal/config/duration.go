package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that unmarshals from a friendly JSON string
// ("30m", "2h") or a bare number of seconds — so config files stay human-edited.
type Duration time.Duration

// Duration returns the underlying time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// OrDefault returns def when the duration is zero (unset).
func (d Duration) OrDefault(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*d = 0
		return nil
	}
	if v, err := time.ParseDuration(s); err == nil {
		*d = Duration(v)
		return nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		*d = Duration(time.Duration(n * float64(time.Second)))
		return nil
	}
	return fmt.Errorf("invalid duration %q", s)
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}
