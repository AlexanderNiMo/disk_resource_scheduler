// Package web serves the interface the daemon is approved through.
package web

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/scheduler"
)

// Server is the web interface.
type Server struct {
	cfg     *config.Store
	log     *slog.Logger
	sch     *scheduler.Scheduler
	history *scheduler.History
	tmpl    *templates

	mu     sync.Mutex
	tokens map[string]session
}

type session struct {
	user string
	// hash is the hash of the password the session was made with. A
	// changed password ends the sessions of that user.
	hash    string
	expires time.Time
	// fragment is the hash of the last fragment sent to this browser.
	// The plan block is polled every few seconds, and a poll that throws
	// the buttons away makes a click on them do nothing.
	fragment string
}

// New builds the web interface.
func New(cfg *config.Store, log *slog.Logger, sch *scheduler.Scheduler, history *scheduler.History) (*Server, error) {
	tmpl, err := newTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:     cfg,
		log:     log,
		sch:     sch,
		history: history,
		tmpl:    tmpl,
		tokens:  map[string]session{},
	}, nil
}

// Handler returns the router.
//
// Everything except the login and the static files is behind requireAuth, so
// a route added later is authenticated by default rather than by whoever
// remembers to wrap it.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.Handle("GET /{$}", s.auth(s.handleDashboard))
	mux.Handle("GET /fragments/plan", s.auth(s.handlePlanFragment))

	// The same handlers serve the pages and the API: the answer is
	// JSON for the API and a page otherwise.
	mux.Handle("POST /scan", s.auth(s.handleScan))
	mux.Handle("POST /plan/approve", s.auth(s.handleApprove))
	mux.Handle("POST /plan/reject", s.auth(s.handleReject))
	mux.Handle("POST /plan/rollback", s.auth(s.handleRollback))

	mux.Handle("GET /api/plan", s.auth(s.apiPlan))
	mux.Handle("GET /api/history", s.auth(s.apiHistory))
	mux.Handle("POST /api/scan", s.auth(s.handleScan))
	mux.Handle("POST /api/plan/approve", s.auth(s.handleApprove))
	mux.Handle("POST /api/plan/reject", s.auth(s.handleReject))
	mux.Handle("POST /api/plan/rollback", s.auth(s.handleRollback))

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.tmpl.static))))

	return s.accessLog(s.sessions(mux))
}

// auth puts the check for a session in front of a handler.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return s.requireAuth(next)
}

// accessLog writes one line per request.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The page asks for the plan every few seconds. A line per poll
		// would bury everything else in the log.
		if r.URL.Path == "/fragments/plan" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration", time.Since(started).Round(time.Millisecond).String(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
