package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
