package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/disk"
	"github.com/alex/disk_resource_scheduler/internal/scheduler"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server, _, _ := newTestServerWith(t)
	return server
}

// newTestServerWith builds the interface and hands back the server and the
// scheduler, so that a test can look at what they did.
func newTestServerWith(t *testing.T) (*httptest.Server, *Server, *scheduler.Scheduler) {
	t.Helper()

	cfg := &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Web:    config.Web{Users: map[string]string{"alex": "secret"}},
		Folders: map[string]config.Folder{
			"/media8/serials": {Path: "/media8/serials", DirType: config.DirTypeSrc, Type: 2, MinFree: 50 << 30},
			"/media5/serials": {Path: "/media5/serials", DirType: config.DirTypeDst, Type: 2},
		},
	}
	store := config.NewStore(cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	history := scheduler.NewHistory(t.TempDir())
	sch := scheduler.New(store, logger, schedulerProbe{}, scheduler.NewStateStore(t.TempDir()), history)

	server, err := New(store, logger, sch, history)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer, server, sch
}

// schedulerProbe is a probe that knows nothing; the tests do not scan.
type schedulerProbe struct{}

func (schedulerProbe) VolumeOf(string) (disk.Volume, error) {
	return disk.Volume{}, nil
}

// testClient is a client with a cookie jar that does not follow redirects:
// the tests look at the redirect itself.
func testClient() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// login goes through the login form and returns a client with a session.
func login(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	client := testClient()

	resp, err := client.Get(server.URL + "/login")
	if err != nil {
		t.Fatalf("get the login page: %v", err)
	}
	resp.Body.Close()

	token := ""
	for _, cookie := range resp.Cookies() {
		if cookie.Name == csrfCookie {
			token = cookie.Value
		}
	}
	if token == "" {
		t.Fatal("the login page did not hand out a csrf token")
	}

	resp, err = client.PostForm(server.URL+"/login", url.Values{
		"user":       {"alex"},
		"password":   {"secret"},
		"csrf_token": {token},
	})
	if err != nil {
		t.Fatalf("post the login form: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the login returned %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	return client
}

func TestTheDashboardNeedsASession(t *testing.T) {
	server := newTestServer(t)

	resp, err := testClient().Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the dashboard returned %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	if location := resp.Header.Get("Location"); !strings.HasSuffix(location, "/login") {
		t.Fatalf("the dashboard redirected to %q, want the login page", location)
	}
}

func TestALoginWithAWrongPasswordIsRefused(t *testing.T) {
	server := newTestServer(t)
	client := testClient()

	resp, err := client.Get(server.URL + "/login")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	token := ""
	for _, cookie := range resp.Cookies() {
		if cookie.Name == csrfCookie {
			token = cookie.Value
		}
	}

	resp, err = client.PostForm(server.URL+"/login", url.Values{
		"user":       {"alex"},
		"password":   {"wrong"},
		"csrf_token": {token},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the login returned %d, want %d", resp.StatusCode, http.StatusOK)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == sessionCookie {
			t.Fatal("a wrong password still got a session")
		}
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Неверный логин или пароль") {
		t.Fatalf("the page does not say what went wrong: %s", body)
	}
}

func TestTheDashboardShowsTheFolders(t *testing.T) {
	server := newTestServer(t)
	client := login(t, server)

	resp, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the dashboard returned %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "План перемещения") {
		t.Fatalf("the dashboard does not show the plan: %s", body)
	}
}

func TestAPostWithoutACsrfTokenIsRefused(t *testing.T) {
	server := newTestServer(t)
	client := login(t, server)

	resp, err := client.PostForm(server.URL+"/scan", url.Values{})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the post returned %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestTheApiNeedsASession(t *testing.T) {
	server := newTestServer(t)

	resp, err := testClient().Get(server.URL + "/api/plan")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	// The API answers with a status, not with a page: a script that follows
	// the redirect would get the login page instead of an answer.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the api returned %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestTheApiAnswersWithJson(t *testing.T) {
	server := newTestServer(t)
	client := login(t, server)

	resp, err := client.Get(server.URL + "/api/plan")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the api returned %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("the api answered with %q", contentType)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "\"state\"") {
		t.Fatalf("the api answered with %s", body)
	}
}

func TestTheDashboardSaysWhenNothingHasBeenScanned(t *testing.T) {
	server := newTestServer(t)
	client := login(t, server)

	resp, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Сканирование ещё не выполнялось") {
		t.Fatalf("the dashboard does not say that nothing was scanned: %s", body)
	}
	if !strings.Contains(string(body), "не измерено") {
		t.Fatalf("the folders do not say that they were not measured: %s", body)
	}
}

func TestApprovingBeforeTheFirstScanIsRefused(t *testing.T) {
	server := newTestServer(t)
	client := login(t, server)

	page, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse the url: %v", err)
	}
	token := ""
	for _, cookie := range client.Jar.Cookies(page) {
		if cookie.Name == csrfCookie {
			token = cookie.Value
		}
	}

	resp, err := client.PostForm(server.URL+"/api/plan/approve", url.Values{
		"plan_id":    {"1"},
		"csrf_token": {token},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("the approval returned %d, want %d", resp.StatusCode, http.StatusConflict)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not been scanned") {
		t.Fatalf("the answer is %s, want it to say that nothing was scanned", body)
	}
}

func TestTheManualScanMeasuresEveryFolder(t *testing.T) {
	root := t.TempDir()
	folders := map[string]config.Folder{}
	for i := 0; i < 5; i++ {
		dir := filepath.Join(root, "src"+strconv.Itoa(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		folders[dir] = config.Folder{Path: dir, DirType: config.DirTypeSrc, Type: 2, MinFree: 1}
	}

	cfg := &config.Config{
		Server:  config.Server{Listen: "127.0.0.1:0"},
		Web:     config.Web{Users: map[string]string{"alex": "secret"}},
		Folders: folders,
	}
	store := config.NewStore(cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	history := scheduler.NewHistory(t.TempDir())
	sch := scheduler.New(store, logger, disk.StatfsProbe{}, scheduler.NewStateStore(t.TempDir()), history)

	server, err := New(store, logger, sch, history)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	client := login(t, httpServer)
	page, _ := url.Parse(httpServer.URL)
	token := ""
	for _, cookie := range client.Jar.Cookies(page) {
		if cookie.Name == csrfCookie {
			token = cookie.Value
		}
	}

	resp, err := client.PostForm(httpServer.URL+"/scan", url.Values{"csrf_token": {token}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	// The scan runs after the response: the context of the request is gone
	// by then, and a scan that used it would stop at the first folder.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := sch.ScanStatus()
		if status.Scanned && !status.Running {
			if status.Measured != status.Total {
				t.Fatalf("the scan measured %d of %d folders", status.Measured, status.Total)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the scan never finished")
}

// A scan that is already running is not started again: the library would be
// walked twice for nothing.
func TestASecondScanIsRefused(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Web:    config.Web{Users: map[string]string{"alex": "secret"}},
		Folders: map[string]config.Folder{
			dir: {Path: dir, DirType: config.DirTypeSrc, Type: 2, MinFree: 1},
		},
	}
	store := config.NewStore(cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	history := scheduler.NewHistory(t.TempDir())
	// The probe is slow, so the scan stays in flight.
	sch := scheduler.New(store, logger, slowProbe{}, scheduler.NewStateStore(t.TempDir()), history)
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())

	server, err := New(store, logger, sch, history)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	client := login(t, httpServer)
	page, _ := url.Parse(httpServer.URL)
	token := ""
	for _, cookie := range client.Jar.Cookies(page) {
		if cookie.Name == csrfCookie {
			token = cookie.Value
		}
	}

	// Wait for the first scan to be under way.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sch.ScanStatus().Running {
		time.Sleep(10 * time.Millisecond)
	}
	if !sch.ScanStatus().Running {
		t.Fatal("the first scan never started")
	}

	resp, err := client.PostForm(httpServer.URL+"/api/scan", url.Values{"csrf_token": {token}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("the second scan returned %d, want %d", resp.StatusCode, http.StatusConflict)
	}
}

// slowProbe measures the volumes slowly, so that a scan stays in flight.
type slowProbe struct{}

func (slowProbe) VolumeOf(string) (disk.Volume, error) {
	time.Sleep(200 * time.Millisecond)
	return disk.Volume{Dev: 1, Free: 1 << 30}, nil
}

func TestASessionOfARemovedUserStopsWorking(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Web:    config.Web{Users: map[string]string{"alex": "secret"}},
	}
	store := config.NewStore(cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	history := scheduler.NewHistory(t.TempDir())
	sch := scheduler.New(store, logger, schedulerProbe{}, scheduler.NewStateStore(t.TempDir()), history)

	server, err := New(store, logger, sch, history)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	client := login(t, httpServer)

	// The user is taken out of the config: the config is the list of the
	// people who may approve the moves.
	store.Store(&config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Web:    config.Web{Users: map[string]string{"bob": "other"}},
	})

	resp, err := client.Get(httpServer.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the dashboard returned %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
}

func TestASessionEndsWithItsTime(t *testing.T) {
	server, srv, _ := newTestServerWith(t)
	client := login(t, server)

	// Reach into the store and make the session older than it may be.
	srv.mu.Lock()
	for token, found := range srv.tokens {
		found.expires = time.Now().Add(-time.Minute)
		srv.tokens[token] = found
	}
	srv.mu.Unlock()

	resp, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the dashboard returned %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
}

func TestAChangedPasswordEndsTheSession(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Web:    config.Web{Users: map[string]string{"alex": "secret"}},
	}
	store := config.NewStore(cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	history := scheduler.NewHistory(t.TempDir())
	sch := scheduler.New(store, logger, schedulerProbe{}, scheduler.NewStateStore(t.TempDir()), history)

	server, err := New(store, logger, sch, history)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	client := login(t, httpServer)

	// The password is changed: the session that was made with the old one
	// must stop working.
	store.Store(&config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Web:    config.Web{Users: map[string]string{"alex": "another"}},
	})

	resp, err := client.Get(httpServer.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the dashboard returned %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
}
