package web

import (
	"encoding/json"
	"net/http"
)

// The API is the same daemon as the interface, seen from a script: the same
// session, the same CSRF token, the same actions.

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

// apiScan starts a scan.
func (s *Server) apiScan(w http.ResponseWriter, r *http.Request) {
	go func() {
		if err := s.sch.Scan(r.Context()); err != nil {
			s.log.Error("the scan failed", "error", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "scanning"})
}

// apiApprove starts the execution of the plan.
func (s *Server) apiApprove(w http.ResponseWriter, r *http.Request) {
	if err := s.sch.Approve(formInt(r, "plan_id")); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "approved"})
}

// apiReject throws the plan away.
func (s *Server) apiReject(w http.ResponseWriter, r *http.Request) {
	if err := s.sch.Reject(formInt(r, "plan_id")); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// apiRollback puts one move back, or all of them when move_id is zero.
func (s *Server) apiRollback(w http.ResponseWriter, r *http.Request) {
	if err := s.sch.Rollback(formInt(r, "plan_id"), formInt(r, "move_id")); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "rolling back"})
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
