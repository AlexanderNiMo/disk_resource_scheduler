package web

import (
	"encoding/json"
	"net/http"
)

// The API is the same daemon as the interface, seen from a script: the same
// session, the same CSRF token, the same actions. The write actions are the
// handlers of the pages; only the two reads are here.

// apiPlan returns the current state, the plan and the history.
func (s *Server) apiPlan(w http.ResponseWriter, r *http.Request) {
	state, plan := s.sch.Snapshot()
	history, err := s.history.List()
	if err != nil {
		s.log.Warn("could not read the history", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":   state,
		"plan":    plan,
		"history": history,
	})
}

// apiHistory returns the finished plans.
func (s *Server) apiHistory(w http.ResponseWriter, r *http.Request) {
	history, err := s.history.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, history)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already written, so there is nothing to say to
		// the client beyond what the log says.
		return
	}
}
