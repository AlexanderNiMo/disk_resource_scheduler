package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Size is a byte count.
//
// In the configuration file it is written as a human string: "50GB".
type Size int64

// units are the suffixes a size may carry. Every one of them is a power of
// 1024, so "1GB" is 1024^3 bytes — the same arithmetic the interface uses
// when it prints a size back.
var units = map[string]int64{
	"":    1,
	"b":   1,
	"k":   1 << 10,
	"kb":  1 << 10,
	"kib": 1 << 10,
	"m":   1 << 20,
	"mb":  1 << 20,
	"mib": 1 << 20,
	"g":   1 << 30,
	"gb":  1 << 30,
	"gib": 1 << 30,
	"t":   1 << 40,
	"tb":  1 << 40,
	"tib": 1 << 40,
	"p":   1 << 50,
	"pb":  1 << 50,
	"pib": 1 << 50,
}

// ParseSize parses a human-readable size: "50GB", "1.5 GB", "512MiB", "100".
func ParseSize(s string) (Size, error) {
	raw := strings.ToLower(strings.TrimSpace(s))
	if raw == "" {
		return 0, fmt.Errorf("want a size like \"50GB\", got %q", s)
	}
	if strings.HasPrefix(raw, "-") {
		return 0, fmt.Errorf("want a positive size, got %q", s)
	}

	digits := 0
	for digits < len(raw) && (raw[digits] >= '0' && raw[digits] <= '9' || raw[digits] == '.') {
		digits++
	}
	number, unit := raw[:digits], strings.TrimSpace(raw[digits:])
	if number == "" {
		return 0, fmt.Errorf("want a size like \"50GB\", got %q", s)
	}
	multiplier, ok := units[unit]
	if !ok {
		return 0, fmt.Errorf("want a size like \"50GB\", got %q", s)
	}
	value, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, fmt.Errorf("want a size like \"50GB\", got %q", s)
	}
	return Size(value * float64(multiplier)), nil
}

// UnmarshalYAML lets a Size be written as "50GB" in the config file.
func (s *Size) UnmarshalYAML(value *yaml.Node) error {
	parsed, err := ParseSize(value.Value)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// Duration is a time span.
//
// yaml.v3 does not parse durations on its own, so the type carries its own
// UnmarshalYAML.
type Duration time.Duration

// UnmarshalYAML lets a Duration be written as "24h" in the config file.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("want a duration like \"24h\", got %q", value.Value)
	}
	*d = Duration(parsed)
	return nil
}
