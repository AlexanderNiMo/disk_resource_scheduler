package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// unmarshalYAML decodes a small YAML document into a value.
func unmarshalYAML(doc string, into any) error {
	return yaml.Unmarshal([]byte(doc), into)
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		fail bool
	}{
		{in: "100", want: 100},
		{in: "50GB", want: 50 << 30},
		{in: "50 gb", want: 50 << 30},
		{in: "1.5GB", want: 1610612736},
		{in: "512MiB", want: 512 << 20},
		{in: "1TB", want: 1 << 40},
		{in: "", fail: true},
		{in: "abc", fail: true},
		{in: "-1GB", fail: true},
		{in: "50GBB", fail: true},
		{in: "1.5.2GB", fail: true},
		{in: "GB", fail: true},
	}

	for _, tc := range cases {
		got, err := ParseSize(tc.in)
		if tc.fail {
			if err == nil {
				t.Errorf("ParseSize(%q): want an error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tc.in, err)
			continue
		}
		if int64(got) != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, int64(got), tc.want)
		}
	}
}

func TestSizeUnmarshalYAML(t *testing.T) {
	cfg := struct {
		MinFree Size `yaml:"min_free"`
	}{}
	if err := unmarshalYAML("min_free: 50GB", &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if int64(cfg.MinFree) != 50<<30 {
		t.Fatalf("min_free = %d, want %d", int64(cfg.MinFree), int64(50<<30))
	}

	if err := unmarshalYAML("min_free: nonsense", &cfg); err == nil {
		t.Fatal("want an error for a size that cannot be read")
	}
}

func TestDurationUnmarshalYAML(t *testing.T) {
	cfg := struct {
		MinAge Duration `yaml:"min_age"`
	}{}
	if err := unmarshalYAML("min_age: 24h", &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if time.Duration(cfg.MinAge).Hours() != 24 {
		t.Fatalf("min_age = %s, want 24h", time.Duration(cfg.MinAge))
	}
	if err := unmarshalYAML("min_age: soon", &cfg); err == nil {
		t.Fatal("want an error for a duration that cannot be read")
	}
}
