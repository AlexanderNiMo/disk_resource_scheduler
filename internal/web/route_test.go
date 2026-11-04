package web

import (
	"net/http/httptest"
	"testing"
)

// The router sends every request to the right place, and turns away the
// ones that have no session.
func TestTheRoutesAreTheRightOnes(t *testing.T) {
	_, srv, _ := newTestServerWith(t)

	cases := []struct {
		method string
		path   string
		want   int
	}{
		{"GET", "/login", 200},
		{"GET", "/", 303},
		{"GET", "/api/plan", 401},
		{"GET", "/fragments/plan", 303},
		{"GET", "/nonsense", 404},
		// The csrf check runs before the router, so a post without a token
		// is turned away whatever the path is.
		{"POST", "/nonsense", 403},
		{"GET", "/api/nonsense", 404},
		{"GET", "/static/style.css", 200},
		{"GET", "/static/nonsense.css", 404},
		{"PUT", "/login", 403},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		got := rec.Code
		mark := "ok "
		if got != c.want {
			mark = "BAD"
			t.Errorf("%s %s: got %d, want %d", c.method, c.path, got, c.want)
		}
		t.Logf("%s %s %s -> %d (want %d)", mark, c.method, c.path, got, c.want)
	}
}
