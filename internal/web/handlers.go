package web

import (
	"net/http"
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
	s.render(w, r, "dashboard", s.buildView(r))
}

// handlePlanFragment is the part of the page that is polled.
func (s *Server) handlePlanFragment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.renderFragment(w, r, "plan_block", s.buildView(r))
}

// handleScan starts a scan.
//
// The scan walks the library, which takes a while, so it runs in the
// background: the page shows the plan as soon as it is ready.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	go func() {
		if err := s.sch.Scan(r.Context()); err != nil {
			s.log.Error("the scan failed", "error", err)
		}
	}()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleApprove starts the execution of the plan.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	err := s.sch.Approve(formInt(r, "plan_id"))
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleReject throws the plan away.
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	err := s.sch.Reject(formInt(r, "plan_id"))
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleRollback puts one move back, or all of them when move_id is zero.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	err := s.sch.Rollback(formInt(r, "plan_id"), formInt(r, "move_id"))
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// renderError shows the dashboard with the error on it.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, err error) {
	view := s.buildView(r)
	view.Error = err.Error()
	s.render(w, r, "dashboard", view)
}
