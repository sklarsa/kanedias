package server

import (
	"fmt"

	"github.com/sklarsa/kanedias/internal/manager"
	"github.com/sklarsa/kanedias/internal/supervisor"
)

// Template names — each corresponds to a fragment file.
const (
	templateFleet      = "fleet.html"
	templateDetail     = "detail.html"
	templateQuestions  = "questions.html"
	templateActivity   = "activity.html"
	templateDeckStatus = "deck-status.html"
)

// fleetView is the template data for fleet.html.
type fleetView struct {
	Roots  []rootView
	Issues []issueView
}

// rootView is the template data for one admitted root.
type rootView struct {
	RootSessionID   string
	Lifecycle       string
	Stale           bool
	StreamConnected bool
	Incomplete      bool
	GapText         string
	Children        []nodeView
}

// nodeView represents one node in the session tree.
type nodeView struct {
	SessionID   string
	WorkerType  string
	Lifecycle   string
	Questions   []questionSummaryView
	Children    []nodeView
	HasChildren bool
}

// issueView represents a discovery issue.
type issueView struct {
	SocketName string
	Code       string
	Message    string
}

// questionSummaryView is the safe projection of one pending question.
type questionSummaryView struct {
	ID          string
	Method      string
	Title       string
	Options     []string
	Message     string
	Placeholder string
	Prefill     string
}

// detailView is the template data for detail.html.
type detailView struct {
	SessionID       string
	WorkerType      string
	Lifecycle       string
	RootSessionID   string
	RootStale       bool
	StreamConnected bool
	Incomplete      bool
	GapText         string
}

// questionPanelView is the template data for questions.html.
type questionPanelView struct {
	SessionID string
	Questions []questionSummaryView
}

// activityView is the template data for activity.html.
type activityView struct {
	SessionID  string
	Items      []activityItemView
	Incomplete bool
	GapText    string
}

// activityItemView is the safe projection of one activity item.
type activityItemView struct {
	Seq      uint64
	Kind     string
	Label    string
	Text     string
	ToolName string
	IsError  bool
}

// deckStatusView is the template data for deck-status.html.
type deckStatusView struct {
	Error   string
	Success bool
}

// contextView holds nullable context metrics.
// Used by newStatsView which is wired in Task 9 action handlers.
type contextView struct { //nolint:unused
	HasPercent bool
	Percent    float64
	HasTokens  bool
	Tokens     float64
}

// statsView is the template data for session stats.
// Used by newStatsView which is wired in Task 9 action handlers.
type statsView struct { //nolint:unused
	TotalMessages     int
	UserMessages      int
	AssistantMessages int
	ToolCalls         int
	ToolResults       int
	Cost              float64
	Context           contextView
	HasStats          bool
}

// newFleetView converts a FleetSnapshot into the template data.
func newFleetView(snap manager.FleetSnapshot) fleetView {
	roots := make([]rootView, 0, len(snap.Roots))
	for _, r := range snap.Roots {
		roots = append(roots, newRootView(r))
	}
	issues := make([]issueView, 0, len(snap.Issues))
	for _, i := range snap.Issues {
		issues = append(issues, issueView{
			SocketName: i.SocketName,
			Code:       i.Code,
			Message:    i.Message,
		})
	}
	return fleetView{Roots: roots, Issues: issues}
}

func newRootView(r manager.RootState) rootView {
	gapText := ""
	if r.Gap != nil {
		gapText = fmt.Sprintf("replay gap: expected seq %d, first available %d",
			r.Gap.ExpectedSeq, r.Gap.FirstAvailableSeq)
	}
	children := nodeChildren(r.Tree.Children)
	return rootView{
		RootSessionID:   r.RootSessionID,
		Lifecycle:       r.Tree.Lifecycle,
		Stale:           r.Stale,
		StreamConnected: r.StreamConnected,
		Incomplete:      r.Incomplete,
		GapText:         gapText,
		Children:        children,
	}
}

func nodeChildren(nodes []supervisor.NodeSnapshot) []nodeView {
	views := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		views = append(views, newNodeView(n))
	}
	return views
}

func newNodeView(n supervisor.NodeSnapshot) nodeView {
	questions := make([]questionSummaryView, 0, len(n.Questions))
	for _, q := range n.Questions {
		questions = append(questions, newQuestionSummaryView(q))
	}
	children := nodeChildren(n.Children)
	return nodeView{
		SessionID:   n.SessionID,
		WorkerType:  n.WorkerType,
		Lifecycle:   n.Lifecycle,
		Questions:   questions,
		Children:    children,
		HasChildren: len(children) > 0,
	}
}

func newQuestionSummaryView(q supervisor.QuestionSummary) questionSummaryView {
	return questionSummaryView{
		ID:          q.ID,
		Method:      q.Method,
		Title:       q.Title,
		Options:     q.Options,
		Message:     q.Message,
		Placeholder: q.Placeholder,
		Prefill:     q.Prefill,
	}
}

// newDetailView converts a SessionState into the detail template data.
func newDetailView(state manager.SessionState) detailView {
	gapText := ""
	if state.Gap != nil {
		gapText = fmt.Sprintf("replay gap: expected seq %d, first available %d",
			state.Gap.ExpectedSeq, state.Gap.FirstAvailableSeq)
	}
	return detailView{
		SessionID:       state.Node.SessionID,
		WorkerType:      state.Node.WorkerType,
		Lifecycle:       state.Node.Lifecycle,
		RootSessionID:   state.RootSessionID,
		RootStale:       state.RootStale,
		StreamConnected: state.StreamConnected,
		Incomplete:      state.Incomplete,
		GapText:         gapText,
	}
}

// newQuestionPanelView converts a SessionState into the question panel data.
func newQuestionPanelView(state manager.SessionState) questionPanelView {
	questions := make([]questionSummaryView, 0, len(state.Node.Questions))
	for _, q := range state.Node.Questions {
		questions = append(questions, newQuestionSummaryView(q))
	}
	return questionPanelView{
		SessionID: state.Node.SessionID,
		Questions: questions,
	}
}

// newActivityView converts a SessionState into the activity panel data.
func newActivityView(state manager.SessionState) activityView {
	items := make([]activityItemView, 0, len(state.RecentActivity))
	for _, a := range state.RecentActivity {
		items = append(items, activityItemView{
			Seq:      a.Seq,
			Kind:     a.Kind,
			Label:    a.Label,
			Text:     a.Text,
			ToolName: a.ToolName,
			IsError:  a.IsError,
		})
	}
	gapText := ""
	if state.Gap != nil {
		gapText = fmt.Sprintf("replay gap: expected seq %d, first available %d",
			state.Gap.ExpectedSeq, state.Gap.FirstAvailableSeq)
	}
	return activityView{
		SessionID:  state.Node.SessionID,
		Items:      items,
		Incomplete: state.Incomplete,
		GapText:    gapText,
	}
}

// newStatsView converts SessionStats into the stats view.
// Wired in Task 9 action handlers.
//
//nolint:unused
func newStatsView(stats manager.SessionStats) statsView {
	ctx := contextView{}
	if stats.ContextUsage != nil {
		if stats.ContextUsage.Percent != nil {
			ctx.HasPercent = true
			ctx.Percent = *stats.ContextUsage.Percent
		}
		if stats.ContextUsage.Tokens != nil {
			ctx.HasTokens = true
			ctx.Tokens = *stats.ContextUsage.Tokens
		}
	}
	return statsView{
		TotalMessages:     stats.TotalMessages,
		UserMessages:      stats.UserMessages,
		AssistantMessages: stats.AssistantMessages,
		ToolCalls:         stats.ToolCalls,
		ToolResults:       stats.ToolResults,
		Cost:              stats.Cost,
		Context:           ctx,
		HasStats:          true,
	}
}

// emptySessionState returns a zero-value SessionState for rendering empty panels.
func emptySessionState() manager.SessionState {
	return manager.SessionState{}
}

// newDeckStatusView converts an operation error into a deck status view.
func newDeckStatusView(err error) deckStatusView {
	if err == nil {
		return deckStatusView{Success: true}
	}
	return deckStatusView{Error: operatorMessage(err)}
}

// operatorMessage converts an internal error to a safe operator-facing message.
// Typed contract errors map to stable copy; all others produce a generic message.
func operatorMessage(err error) string {
	if err == nil {
		return ""
	}
	// Contract errors are handled in Task 9 with typed switch.
	// This base implementation provides the generic fallback.
	return "The supervisor command could not be completed."
}
