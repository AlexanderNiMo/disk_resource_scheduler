package web

import (
	"fmt"
	"net/http"
	"strings"
)

// handleLoginForm shows the login page.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", s.buildLoginView(r))
}

// handleLogin checks the credentials and starts a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("user")
	password := r.PostFormValue("password")
	if !s.checkCredentials(name, password) {
		s.log.Warn("a login failed", "user", name, "remote", r.RemoteAddr)
		view := s.buildLoginView(r)
		view.Error = "Неверный логин или пароль"
		s.render(w, r, "login", view)
		return
	}

	token := s.newSession(name)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies(),
		MaxAge:   int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout drops the session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.dropSession(cookie.Value)
	}
	s.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleDashboard shows the folders, the plan and the history.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "dashboard", s.buildView(r, true))
}

// handlePlanFragment is the part of the page that is polled.
//
// The history is not in the fragment, so it is not read: the file holds
// every plan ever made, and reading it every few seconds would cost more
// than the rest of the page.
func (s *Server) handlePlanFragment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.renderFragment(w, r, "plan_block", s.buildView(r, false))
}

// handleScan starts a scan.
//
// The scan walks the library, which takes a while, so it runs in the
// background: the page shows the plan as soon as it is ready.
//
// A scan that is already running is not started again: the library would
// be walked twice for nothing.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if err := s.sch.StartScan(); err != nil {
		s.fail(w, r, err)
		return
	}
	s.answer(w, r, http.StatusAccepted, map[string]string{"status": "scanning"})
}

// handleApprove starts the execution of the plan.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	planID, err := formInt(r, "plan_id")
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	if err := s.sch.Approve(planID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.answer(w, r, http.StatusAccepted, map[string]string{"status": "approved"})
}

// handleReject throws the plan away.
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	planID, err := formInt(r, "plan_id")
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	if err := s.sch.Reject(planID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.answer(w, r, http.StatusOK, map[string]string{"status": "rejected"})
}

// handleRollback puts one move back, or all of them when move_id is zero.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	planID, err := formInt(r, "plan_id")
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	moveID, err := formInt(r, "move_id")
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	if moveID < 0 {
		s.badRequest(w, r, fmt.Errorf("move_id: want zero or a number, got %d", moveID))
		return
	}
	if err := s.sch.Rollback(planID, moveID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.answer(w, r, http.StatusAccepted, map[string]string{"status": "rolling back"})
}

// answer finishes a request: JSON for the API, a page otherwise.
func (s *Server) answer(w http.ResponseWriter, r *http.Request, status int, payload map[string]string) {
	if isAPI(r) {
		writeJSON(w, status, payload)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// fail reports an error: JSON for the API, the page with the error on it
// otherwise.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if isAPI(r) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	view := s.buildView(r, true)
	view.Error = err.Error()
	s.render(w, r, "dashboard", view)
}

// badRequest reports a form that cannot be read.
//
// It is a different answer from fail: the request was never understood,
// so it is not a conflict with what the daemon is doing.
func (s *Server) badRequest(w http.ResponseWriter, r *http.Request, err error) {
	if isAPI(r) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	view := s.buildView(r, true)
	view.Error = err.Error()
	s.render(w, r, "dashboard", view)
}

// isAPI tells whether the request wants JSON instead of a page.
func isAPI(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/")
}
