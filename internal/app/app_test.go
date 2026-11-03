package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/disk"
)

// writeConfig puts a configuration file into a temporary directory.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	return path
}

// canLogIn says whether the given user gets a session.
func canLogIn(t *testing.T, addr, user, password string) bool {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{
		Jar:     jar,
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get("http://" + addr + "/login")
	if err != nil {
		return false
	}
	resp.Body.Close()

	token := ""
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "drs_csrf" {
			token = cookie.Value
		}
	}
	if token == "" {
		return false
	}

	resp, err = client.PostForm("http://"+addr+"/login", url.Values{
		"user":       {user},
		"password":   {password},
		"csrf_token": {token},
	})
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusSeeOther
}

func configBody(stateDir, users string) string {
	return fmt.Sprintf(`
log:
  level: error
server:
  listen: "127.0.0.1:0"
scan:
  schedule: "@every 1h"
  min_age: "24h"
state:
  dir: %q
web:
  users:
%s
folders:
  /media8/serials:
    dir_type: src
    type: 2
    min_free: 50GB
  /media5/serials:
    dir_type: dst
    type: 2
`, stateDir, users)
}

func TestTheConfigIsReloaded(t *testing.T) {
	stateDir := t.TempDir()
	path := writeConfig(t, configBody(stateDir, "    alex: \"secret\"\n"))
	daemon, err := New(path, WithPollInterval(20*time.Millisecond))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := daemon.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { t0 := time.Now(); daemon.Stop(context.Background()); t.Logf("stop took %s", time.Since(t0)) }()

	addr := daemon.Addr()
	t0 := time.Now()
	if !canLogIn(t, addr, "alex", "secret") {
		t.Fatal("the first user cannot log in")
	}
	t.Logf("first login took %s", time.Since(t0))

	// A second user is added to the config file.
	if err := os.WriteFile(path, []byte(configBody(stateDir, "    alex: \"secret\"\n    bob: \"other\"\n")), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if canLogIn(t, addr, "bob", "other") {
			t.Logf("bob logged in after %s", time.Since(t0))
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the new user never appeared")
}

func TestABrokenConfigDoesNotStopTheDaemon(t *testing.T) {
	path := writeConfig(t, configBody(t.TempDir(), "    alex: \"secret\"\n"))
	daemon, err := New(path, WithPollInterval(20*time.Millisecond))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := daemon.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { t0 := time.Now(); daemon.Stop(context.Background()); t.Logf("stop took %s", time.Since(t0)) }()

	addr := daemon.Addr()
	if err := os.WriteFile(path, []byte("folders: [ this is not a config"), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if !canLogIn(t, addr, "alex", "secret") {
		t.Fatal("a broken config took the daemon down")
	}
}

func TestTheListenAddressIsChecked(t *testing.T) {
	// A config without a port is refused at startup, not at the first request.
	path := writeConfig(t, strings.Replace(configBody(t.TempDir(), "    alex: \"secret\"\n"),
		`listen: "127.0.0.1:0"`, `listen: "no port here"`, 1))
	if _, err := New(path); err == nil {
		t.Fatal("want an error for a listen address without a port")
	}
}

// slowProbe measures one folder slowly, so that the daemon can be stopped
// while its startup scan is walking.
type slowProbe struct {
	slow  string
	delay time.Duration
	devs  map[string]uint64
}

func (p slowProbe) VolumeOf(path string) (disk.Volume, error) {
	if path == p.slow {
		time.Sleep(p.delay)
	}
	return disk.Volume{Dev: p.devs[path], Free: 1 << 30, Total: 10 << 30}, nil
}

// The daemon is stopped while its startup scan is walking. The scan is
// cancelled, and it must not leave a plan behind: the folders it did not
// reach would look like full disks, and the plan would be built from half
// the data.
func TestTheStopDuringTheFirstScanLeavesNoPlan(t *testing.T) {
	root := t.TempDir()
	// The folders are measured in the order of their paths: a-src first,
	// then b-dst, and c-src is never reached.
	src := filepath.Join(root, "a-src")
	dst := filepath.Join(root, "b-dst")
	third := filepath.Join(root, "c-src")
	for _, dir := range []string{src, dst, third} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "state")
	path := writeConfig(t, fmt.Sprintf(`
log:
  level: error
server:
  listen: "127.0.0.1:0"
scan:
  schedule: "@every 1h"
  min_age: "0s"
state:
  dir: %q
web:
  users:
    alex: "secret"
folders:
  %s:
    dir_type: src
    type: 2
    min_free: 900TB
  %s:
    dir_type: dst
    type: 2
  %s:
    dir_type: src
    type: 2
    min_free: 900TB
`, stateDir, src, dst, third))

	probe := slowProbe{slow: dst, delay: 2 * time.Second,
		devs: map[string]uint64{src: 1, dst: 2, third: 3}}
	daemon, err := New(path, WithProbe(probe))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := daemon.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	// The startup scan is walking the destination folder. Stop the daemon.
	time.Sleep(300 * time.Millisecond)
	if err := daemon.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// The measurement is half done, so there is no plan to keep.
	body, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read the state file: %v", err)
		}
		return
	}
	t.Fatalf("the interrupted scan left a plan behind: %s", body)
}
