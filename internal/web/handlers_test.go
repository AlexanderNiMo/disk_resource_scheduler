package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/disk"
	"github.com/alex/disk_resource_scheduler/internal/scheduler"
)

func newTestServer(t *testing.T) *httptest.Server {
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
	return httpServer
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

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the api returned %d, want %d", resp.StatusCode, http.StatusSeeOther)
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
