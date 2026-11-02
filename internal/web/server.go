// Package web serves the interface the daemon is approved through.
package web

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/scheduler"
	"github.com/go-chi/chi/v5"
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
	user    string
	expires time.Time
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
	r := chi.NewRouter()
	r.Use(s.accessLog)
	r.Use(s.sessions)

	r.Get("/login", s.handleLoginForm)
	r.Post("/login", s.handleLogin)
	r.Post("/logout", s.handleLogout)

	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)

		r.Get("/", s.handleDashboard)
		r.Get("/fragments/plan", s.handlePlanFragment)
		r.Post("/scan", s.handleScan)
		r.Post("/plan/approve", s.handleApprove)
		r.Post("/plan/reject", s.handleReject)
		r.Post("/plan/rollback", s.handleRollback)

		r.Get("/api/plan", s.apiPlan)
		r.Get("/api/history", s.apiHistory)
		r.Post("/api/scan", s.apiScan)
		r.Post("/api/plan/approve", s.apiApprove)
		r.Post("/api/plan/reject", s.apiReject)
		r.Post("/api/plan/rollback", s.apiRollback)
	})

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(s.tmpl.static))))
	return r
}

// accessLog writes one line per request.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
