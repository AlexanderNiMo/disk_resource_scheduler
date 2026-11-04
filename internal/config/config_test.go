package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig puts a config file into a temporary directory.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	return path
}

const validConfig = `
folders:
  /media8/serials:
    dir_type: src
    type: 2
    min_free: 50GB
  /media5/serials:
    dir_type: dst
    type: 2
    min_free: 50GB
  /media8/films:
    dir_type: src
    type: 1
    min_free: 10GB
  /media5/films:
    dir_type: dst
    type: 1
web:
  users:
    alex: "secret"
`

func TestLoad(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Folders) != 4 {
		t.Fatalf("got %d folders, want 4", len(cfg.Folders))
	}
	if cfg.Folders["/media8/serials"].Path != "/media8/serials" {
		t.Fatalf("the path was not filled in: %+v", cfg.Folders["/media8/serials"])
	}
	if cfg.Folders["/media8/serials"].MinFree != 50<<30 {
		t.Fatalf("min_free = %d, want %d", int64(cfg.Folders["/media8/serials"].MinFree), int64(50<<30))
	}

	sorted := cfg.Sorted()
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1].Path > sorted[i].Path {
			t.Fatalf("the folders are not sorted: %v", sorted)
		}
	}
}

func TestLoadRejectsAnUnknownField(t *testing.T) {
	_, err := Load(writeConfig(t, strings.Replace(validConfig, "dir_type: src", "dir_typ: src", 1)))
	if err == nil {
		t.Fatal("want an error for a field that does not exist")
	}
	if !strings.Contains(err.Error(), "dir_typ") {
		t.Fatalf("the error should name the field: %v", err)
	}
}

func TestLoadRejectsADuplicatePath(t *testing.T) {
	_, err := Load(writeConfig(t, validConfig+`
  /media8/serials:
    dir_type: dst
    type: 2
`))
	if err == nil {
		t.Fatal("want an error for a path that is defined twice")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no folders",
			body: "web:\n  users:\n    alex: secret\n",
			want: "folders: want at least one folder, got none",
		},
		{
			name: "a bad role",
			body: `
folders:
  /media8:
    dir_type: sorce
    type: 2
    min_free: 50GB
  /media5:
    dir_type: dst
    type: 2
web:
  users:
    alex: secret
`,
			want: `dir_type: want "src" or "dst", got "sorce"`,
		},
		{
			name: "a relative path",
			body: `
folders:
  media8:
    dir_type: src
    type: 2
    min_free: 50GB
  /media5:
    dir_type: dst
    type: 2
web:
  users:
    alex: secret
`,
			want: "want an absolute path",
		},
		{
			name: "a source without a threshold",
			body: `
folders:
  /media8:
    dir_type: src
    type: 2
  /media5:
    dir_type: dst
    type: 2
web:
  users:
    alex: secret
`,
			want: "min_free: want a size for a src folder",
		},
		{
			name: "a type with no destination",
			body: `
folders:
  /media8:
    dir_type: src
    type: 3
    min_free: 50GB
  /media5:
    dir_type: dst
    type: 2
web:
  users:
    alex: secret
`,
			want: "type: want a dst folder with type 3, got none",
		},
		{
			name: "no users",
			body: `
folders:
  /media8:
    dir_type: src
    type: 2
    min_free: 50GB
  /media5:
    dir_type: dst
    type: 2
`,
			want: "web.users: want at least one user, got none",
		},
		{
			name: "an empty password",
			body: `
folders:
  /media8:
    dir_type: src
    type: 2
    min_free: 50GB
  /media5:
    dir_type: dst
    type: 2
web:
  users:
    alex: ""
`,
			want: "web.users.alex: want a password",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error is %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
