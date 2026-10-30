package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that decodes from YAML strings such as "6h" and
// from bare integers meaning seconds.
type Duration time.Duration

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalYAML encodes the duration as its string form.
func (d Duration) MarshalYAML() (any, error) { return d.Std().String(), nil }

// UnmarshalYAML decodes "6h" style strings or integer seconds.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	raw := node.Value
	if raw == "" {
		*d = 0
		return nil
	}
	if parsed, err := time.ParseDuration(raw); err == nil {
		*d = Duration(parsed)
		return nil
	}
	var secs int64
	if _, err := fmt.Sscan(raw, &secs); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	return fmt.Errorf("config: invalid duration %q", raw)
}
