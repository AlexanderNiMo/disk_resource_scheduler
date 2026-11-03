package web

import (
	"net/http"
	"strconv"

	"github.com/alex/disk_resource_scheduler/internal/scheduler"
)

// page is what every template needs from the layout.
//
// Embedded in the view model rather than passed alongside, so that a handler
// returns one value and the layout's fields are at the top level where the
// template expects them.
type page struct {
	Title string
	Flash string
	Error string
	CSRF  string
	User  string
}

// view is the whole dashboard.
type view struct {
	page
	Folders []folderRow
	Plan    planView
	History []historyRow
}

type folderRow struct {
	Path    string
	Role    string
	Type    int
	Free    string
	Total   string
	MinFree string
	State   string
	Error   string
}

type planView struct {
	HasPlan     bool
	PlanID      int
	StateText   string
	Rows        []moveRow
	Unmovable   []unmovableRow
	CanApprove  bool
	CanReject   bool
	CanRollback bool
}

type moveRow struct {
	ID          int
	Name        string
	Size        string
	Src         string
	Dst         string
	Status      string
	Attempt     int
	Error       string
	CanRollback bool
}

type unmovableRow struct {
	Name   string
	Size   string
	Src    string
	Reason string
}

type historyRow struct {
	ID          int
	Kind        string
	CreatedAt   string
	MovesCount  int
	Bytes       string
	State       string
	CanRollback bool
}

// buildView puts the current state into the shape the templates expect.
//
// The view models are separate from the scheduler's types on purpose: adding
// a field to a plan does not change any page until it is added here on
// purpose.
func (s *Server) buildView(r *http.Request) view {
	result := view{page: s.buildPage(r)}

	state, plan := s.sch.Snapshot()
	result.Plan.StateText = stateText(state)

	if plan != nil {
		result.Plan.HasPlan = true
		result.Plan.PlanID = plan.ID
		result.Plan.CanApprove = state == scheduler.StatePending && len(plan.Moves) > 0
		result.Plan.CanReject = state == scheduler.StatePending
		result.Plan.CanRollback = state == scheduler.StateDone || state == scheduler.StateFailed
		if len(plan.Moves) == 0 {
			result.Plan.StateText = "переносить нечего: на всех источниках достаточно свободного места"
		}

		for _, folder := range plan.Folders {
			result.Folders = append(result.Folders, folderRow{
				Path:    folder.Path,
				Role:    roleText(folder.DirType),
				Type:    folder.Type,
				Free:    humanBytes(folder.Free),
				Total:   humanBytes(folder.Total),
				MinFree: humanBytes(folder.MinFree),
				State:   folderStateText(folder.State),
				Error:   folder.Error,
			})
		}
		for _, move := range plan.Moves {
			result.Plan.Rows = append(result.Plan.Rows, moveRow{
				ID:      move.ID,
				Name:    move.Name,
				Size:    humanBytes(move.Size),
				Src:     move.Src,
				Dst:     move.Dst,
				Status:  moveStatusText(move.Status),
				Attempt: move.Attempt,
				Error:   move.Error,
				// Only a finished move can be put back, and only while
				// nothing else is running.
				CanRollback: move.Status == scheduler.StatusDone &&
					(state == scheduler.StateDone || state == scheduler.StateFailed),
			})
		}
		for _, entry := range plan.Unmovable {
			result.Plan.Unmovable = append(result.Plan.Unmovable, unmovableRow{
				Name:   entry.Name,
				Size:   humanBytes(entry.Size),
				Src:    entry.Src,
				Reason: entry.Reason,
			})
		}
	} else {
		for _, folder := range s.cfg.Load().Sorted() {
			result.Folders = append(result.Folders, folderRow{
				Path:    folder.Path,
				Role:    roleText(string(folder.DirType)),
				Type:    folder.Type,
				MinFree: humanBytes(int64(folder.MinFree)),
			})
		}
	}

	entries, err := s.history.List()
	if err != nil {
		s.log.Warn("could not read the history", "error", err)
	}
	for _, entry := range entries {
		result.History = append(result.History, historyRow{
			ID:         entry.ID,
			Kind:       entry.Kind,
			CreatedAt:  entry.CreatedAt.Format("2006-01-02 15:04"),
			MovesCount: entry.MovesCount,
			Bytes:      humanBytes(entry.Bytes),
			State:      entry.State,
			// A finished plan can be put back, even when a newer scan has
			// already replaced the current one.
			CanRollback: entry.Kind == "plan" &&
				(entry.State == string(scheduler.StateDone) || entry.State == string(scheduler.StateFailed)),
		})
	}
	return result
}

func (s *Server) buildPage(r *http.Request) page {
	csrf, _ := r.Context().Value(ctxCSRF).(string)
	user, _ := r.Context().Value(ctxUser).(string)
	return page{
		Title: "Планировщик диска",
		CSRF:  csrf,
		User:  user,
	}
}

func (s *Server) buildLoginView(r *http.Request) view {
	return view{page: s.buildPage(r)}
}

func stateText(state scheduler.State) string {
	switch state {
	case scheduler.StateIdle:
		return "плана нет"
	case scheduler.StatePending:
		return "ждёт одобрения"
	case scheduler.StateRunning:
		return "выполняется"
	case scheduler.StateRollingBack:
		return "откатывается"
	case scheduler.StateDone:
		return "выполнен"
	case scheduler.StateFailed:
		return "выполнен с ошибками"
	}
	return string(state)
}

func roleText(dirType string) string {
	switch dirType {
	case "src":
		return "источник"
	case "dst":
		return "приёмник"
	}
	return dirType
}

func folderStateText(state string) string {
	switch state {
	case "ok":
		return "норма"
	case "low":
		return "мало места"
	case "error":
		return "ошибка"
	}
	return state
}

func moveStatusText(status string) string {
	switch status {
	case scheduler.StatusPending:
		return "в очереди"
	case scheduler.StatusMoving:
		return "переносится"
	case scheduler.StatusDone:
		return "готово"
	case scheduler.StatusFailed:
		return "ошибка"
	case scheduler.StatusRolledBack:
		return "откачено"
	}
	return status
}

// formInt reads a number from the form.
func formInt(r *http.Request, name string) int {
	value, err := strconv.Atoi(r.PostFormValue(name))
	if err != nil {
		return 0
	}
	return value
}
