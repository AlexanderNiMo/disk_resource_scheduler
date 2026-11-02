// Package e2e drives the whole daemon the way a person would: a tree of
// folders, a config file, the daemon itself, and the API it is approved
// through.
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/app"
	"github.com/alex/disk_resource_scheduler/internal/disk"
)

// splitProbe gives every configured folder its own volume.
//
// In a temporary directory every folder is on the same filesystem, and a
// move inside one filesystem frees no space, so the daemon would have nothing
// to do. The files and the moves stay real; only the volume each folder
// belongs to is made up.
type splitProbe struct {
	real disk.Probe
	devs map[string]uint64
}

func (p splitProbe) VolumeOf(path string) (disk.Volume, error) {
	volume, err := p.real.VolumeOf(path)
	if err != nil {
		return volume, err
	}
	if dev, ok := p.devs[path]; ok {
		volume.Dev = dev
	}
	return volume, nil
}

// api is a logged-in client of the daemon's API.
type api struct {
	http *http.Client
	base string
	csrf string
}

type planResponse struct {
	State string `json:"state"`
	Plan  *struct {
		ID    int `json:"id"`
		Moves []struct {
			ID      int    `json:"id"`
			Name    string `json:"name"`
			Path    string `json:"path"`
			Dst     string `json:"dst"`
			DstPath string `json:"dst_path"`
			Status  string `json:"status"`
		} `json:"moves"`
	} `json:"plan"`
}

func (a *api) login(t *testing.T, user, password string) {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	a.http = &http.Client{
		Jar:     jar,
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	a.base = "http://" + a.base

	resp, err := a.http.Get(a.base + "/login")
	if err != nil {
		t.Fatalf("get the login page: %v", err)
	}
	resp.Body.Close()
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "drs_csrf" {
			a.csrf = cookie.Value
		}
	}
	if a.csrf == "" {
		t.Fatal("the login page did not hand out a csrf token")
	}

	resp, err = a.http.PostForm(a.base+"/login", url.Values{
		"user":       {user},
		"password":   {password},
		"csrf_token": {a.csrf},
	})
	if err != nil {
		t.Fatalf("post the login form: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the login returned %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
}

func (a *api) plan(t *testing.T) planResponse {
	t.Helper()

	resp, err := a.http.Get(a.base + "/api/plan")
	if err != nil {
		t.Fatalf("get the plan: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the api returned %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var payload planResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode the plan: %v", err)
	}
	return payload
}

func (a *api) post(t *testing.T, path string, values url.Values) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, a.base+path, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", a.csrf)

	resp, err := a.http.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("post %s returned %d: %s", path, resp.StatusCode, body)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("make %q: %v", path, err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make the directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func TestTheDaemonMovesFilesAfterAnApproval(t *testing.T) {
	root := t.TempDir()

	srcSerials := filepath.Join(root, "media8", "serials")
	srcFilms := filepath.Join(root, "media8", "films")
	dstSerials := filepath.Join(root, "media5", "serials")
	dstFilms := filepath.Join(root, "media6", "films")

	// The library: a directory with two episodes, a file on its own, and a
	// film of another type.
	writeFile(t, filepath.Join(srcSerials, "Show.S01", "ep01.mkv"), "first episode")
	writeFile(t, filepath.Join(srcSerials, "Show.S01", "ep02.mkv"), "second episode")
	writeFile(t, filepath.Join(srcSerials, "Movie.2019.mkv"), "a movie")
	writeFile(t, filepath.Join(srcFilms, "Film.2020", "film.mkv"), "a film")
	mkdirAll(t, dstSerials)
	mkdirAll(t, dstFilms)

	// The threshold is far above the free space of the temporary disk, so
	// everything that may be moved is planned.
	configPath := filepath.Join(root, "config.yaml")
	writeFile(t, configPath, fmt.Sprintf(`
log:
  level: error
server:
  listen: "127.0.0.1:0"
scan:
  schedule: "@every 2s"
  min_age: "0s"
state:
  dir: %q
web:
  users:
    alice: "secret"
folders:
  %s:
    dir_type: src
    type: 2
    min_free: 900TB
  %s:
    dir_type: src
    type: 1
    min_free: 900TB
  %s:
    dir_type: dst
    type: 2
  %s:
    dir_type: dst
    type: 1
`, filepath.Join(root, "state"), srcSerials, srcFilms, dstSerials, dstFilms))

	devs := map[string]uint64{}
	for i, path := range []string{srcSerials, srcFilms, dstSerials, dstFilms} {
		devs[path] = uint64(i + 1)
	}
	probe := splitProbe{real: disk.StatfsProbe{}, devs: devs}

	daemon, err := app.New(configPath, app.WithProbe(probe))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := daemon.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer daemon.Stop(context.Background())

	client := &api{base: daemon.Addr()}
	client.login(t, "alice", "secret")

	// The daemon scans on its own; the plan shows up without anybody asking.
	var plan planResponse
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		plan = client.plan(t)
		if plan.Plan != nil && len(plan.Plan.Moves) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if plan.Plan == nil || len(plan.Plan.Moves) != 3 {
		t.Fatalf("the plan has %+v, want the three entries of the library", plan.Plan)
	}
	if plan.State != "pending" {
		t.Fatalf("the state is %q, want pending", plan.State)
	}

	// Nothing has moved yet: the daemon waits for an approval.
	if _, err := os.Stat(filepath.Join(srcSerials, "Show.S01")); err != nil {
		t.Fatalf("the file moved before the approval: %v", err)
	}

	// The approval goes through the API.
	client.post(t, "/api/plan/approve", url.Values{"plan_id": {strconv.Itoa(plan.Plan.ID)}})

	// The wait is on the files, not on the plan: a scan that runs after the
	// execution replaces the current plan with a new one.
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if moved(t, dstSerials, "Show.S01") && moved(t, dstSerials, "Movie.2019.mkv") &&
			moved(t, dstFilms, "Film.2020") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The serial went to a destination of the same type, the film to its own.
	body, err := os.ReadFile(filepath.Join(dstSerials, "Show.S01", "ep01.mkv"))
	if err != nil || string(body) != "first episode" {
		t.Fatalf("the episode is %q %v, want it at the destination", body, err)
	}
	body, err = os.ReadFile(filepath.Join(dstSerials, "Movie.2019.mkv"))
	if err != nil || string(body) != "a movie" {
		t.Fatalf("the movie is %q %v, want it at the destination", body, err)
	}
	body, err = os.ReadFile(filepath.Join(dstFilms, "Film.2020", "film.mkv"))
	if err != nil || string(body) != "a film" {
		t.Fatalf("the film is %q %v, want it at the destination", body, err)
	}
	// And nothing is left at the source.
	if entries, err := os.ReadDir(srcSerials); err != nil || len(entries) != 0 {
		t.Fatalf("the source still has %v %v", entries, err)
	}
	if entries, err := os.ReadDir(srcFilms); err != nil || len(entries) != 0 {
		t.Fatalf("the source still has %v %v", entries, err)
	}

	// One move is put back; the others stay where they are.
	var target int
	for _, move := range plan.Plan.Moves {
		if move.Name == "Movie.2019.mkv" {
			target = move.ID
		}
	}
	if target == 0 {
		t.Fatal("the movie is not in the plan")
	}
	client.post(t, "/api/plan/rollback", url.Values{
		"plan_id": {strconv.Itoa(plan.Plan.ID)},
		"move_id": {strconv.Itoa(target)},
	})

	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(srcSerials, "Movie.2019.mkv")); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := os.Stat(filepath.Join(srcSerials, "Movie.2019.mkv")); err != nil {
		t.Fatalf("the movie was not put back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstSerials, "Movie.2019.mkv")); err == nil {
		t.Fatal("the movie is still at the destination")
	}
	if _, err := os.Stat(filepath.Join(dstSerials, "Show.S01")); err != nil {
		t.Fatalf("the show was moved as well: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstFilms, "Film.2020")); err != nil {
		t.Fatalf("the film was moved as well: %v", err)
	}

	// The plan is in the history.
	history, err := os.ReadFile(filepath.Join(root, "state", "history.jsonl"))
	if err != nil {
		t.Fatalf("read the history: %v", err)
	}
	if !strings.Contains(string(history), `"moves_count":3`) {
		t.Fatalf("the history does not mention the plan: %s", history)
	}
}

// moved says whether the entry is at the destination.
func moved(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}
