// Package config loads and validates the daemon's configuration.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// DirType says whether a folder gives files away or takes them in.
type DirType string

const (
	DirTypeSrc DirType = "src"
	DirTypeDst DirType = "dst"
)

// Folder is one configured directory.
//
// The path is the key of the map the folder comes from. It is copied into
// Path after decoding, so everything downstream reads it from one place
// instead of carrying the map key around next to the value.
type Folder struct {
	Path    string  `yaml:"-"`
	DirType DirType `yaml:"dir_type"`
	Type    int     `yaml:"type"`
	MinFree Size    `yaml:"min_free"`
}

// Config is the whole configuration file.
type Config struct {
	Log     Log               `yaml:"log"`
	Server  Server            `yaml:"server"`
	Scan    Scan              `yaml:"scan"`
	State   State             `yaml:"state"`
	Web     Web               `yaml:"web"`
	Folders map[string]Folder `yaml:"folders"`
}

// Log is where and how the daemon writes its log.
type Log struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
}

// Server is the web interface's address.
type Server struct {
	Listen  string `yaml:"listen"`
	BaseURL string `yaml:"base_url"`
}

// Scan is when and what the daemon looks at.
type Scan struct {
	Schedule string   `yaml:"schedule"`
	MinAge   Duration `yaml:"min_age"`
}

// State is where the plan and the history live.
type State struct {
	Dir string `yaml:"dir"`
}

// Web holds the users of the web interface.
type Web struct {
	Users map[string]string `yaml:"users"`
}

// defaults is the configuration the file is decoded over, so that a partial
// file keeps everything it does not mention.
func defaults() Config {
	return Config{
		Log:    Log{Level: "info", Format: "text"},
		Server: Server{Listen: "127.0.0.1:8088"},
		Scan:   Scan{Schedule: "*/30 * * * *", MinAge: Duration(24 * time.Hour)},
		State:  State{Dir: "/var/lib/disk-resource-scheduler"},
		Web:    Web{Users: map[string]string{}},
	}
}

// Load reads the configuration file and validates it.
func Load(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close()

	cfg := defaults()
	decoder := yaml.NewDecoder(file)
	// A typo in dir_type has to be an error, not a silently ignored key:
	// this file decides where files go.
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	for path, folder := range cfg.Folders {
		folder.Path = path
		cfg.Folders[path] = folder
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Sorted returns the folders ordered by path.
//
// Go randomises map iteration, so this is the only way the folders are ever
// walked: the scan, the destination choice and the table in the interface all
// use it, and the output is the same on every run.
func (c *Config) Sorted() []Folder {
	folders := make([]Folder, 0, len(c.Folders))
	for _, folder := range c.Folders {
		folders = append(folders, folder)
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].Path < folders[j].Path })
	return folders
}

func (c *Config) validate() error {
	if len(c.Folders) == 0 {
		return fmt.Errorf("folders: want at least one folder, got none")
	}

	dstTypes := map[int]bool{}
	for _, folder := range c.Sorted() {
		switch folder.DirType {
		case DirTypeDst:
			dstTypes[folder.Type] = true
		case DirTypeSrc:
		default:
			return fmt.Errorf("folders.%s.dir_type: want %q or %q, got %q",
				folder.Path, DirTypeSrc, DirTypeDst, folder.DirType)
		}
		if !filepath.IsAbs(folder.Path) {
			return fmt.Errorf("folders.%s: want an absolute path, got %q", folder.Path, folder.Path)
		}
		if folder.Type <= 0 {
			return fmt.Errorf("folders.%s.type: want a positive number, got %d", folder.Path, folder.Type)
		}
		if folder.DirType == DirTypeSrc && folder.MinFree <= 0 {
			return fmt.Errorf("folders.%s.min_free: want a size for a src folder (e.g. \"50GB\"), got 0", folder.Path)
		}
	}
	for _, folder := range c.Sorted() {
		if folder.DirType == DirTypeSrc && !dstTypes[folder.Type] {
			return fmt.Errorf("folders.%s.type: want a dst folder with type %d, got none", folder.Path, folder.Type)
		}
	}

	if len(c.Web.Users) == 0 {
		return fmt.Errorf("web.users: want at least one user, got none")
	}
	names := make([]string, 0, len(c.Web.Users))
	for name := range c.Web.Users {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if c.Web.Users[name] == "" {
			return fmt.Errorf("web.users.%s: want a password, got %q", name, "")
		}
	}

	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		return fmt.Errorf("server.listen: want host:port, got %q", c.Server.Listen)
	}
	if c.Scan.MinAge < 0 {
		return fmt.Errorf("scan.min_age: want a positive duration, got %s", time.Duration(c.Scan.MinAge))
	}
	if c.Scan.Schedule == "" {
		return fmt.Errorf("scan.schedule: want a cron expression, got %q", c.Scan.Schedule)
	}
	return nil
}
