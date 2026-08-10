package server

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/sklarsa/kanedias/internal/manager"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// Template names — each corresponds to a fragment file.
const (
	templateFleet      = "fleet.html"
	templateDetail     = "detail.html"
	templateQuestions  = "questions.html"
	templateActivity   = "activity.html"
	templateDeckStatus = "deck-status.html"
)

// indexView is the template data for index.html.
type indexView struct {
	SessionModal sessionModalView
}

// sessionModalView is the launch configuration rendered into the closed dialog.
type sessionModalView struct {
	Enabled      bool
	Repositories []repositoryOptionView
	RootModels   []modelOptionView
	RootThinking []thinkingOptionView
	Workers      []workerOptionView
}

// repositoryOptionView exposes only the configured slug and whether it is the
// configured browser default. Repository paths and credentials never enter the
// template projection.
type repositoryOptionView struct {
	Slug     string
	Selected bool
}

// modelOptionView intentionally exposes only allowlisted browser-facing model
// metadata. Raw provider and model identifiers never enter this projection.
type modelOptionView struct {
	ID                   string
	Label                string
	ThinkingLevelsCSV    string
	DefaultThinkingLevel string
	Selected             bool
}

type thinkingOptionView struct {
	Level    string
	Selected bool
}

type workerOptionView struct {
	Index       int
	Role        string
	Description string
	Models      []modelOptionView
	Thinking    []thinkingOptionView
}

// newIndexView projects immutable manager launch options into deterministic
// template rows. Production launch options are validated and nonempty.
func newIndexView(options manager.SessionLaunchOptions) indexView {
	models := append([]manager.ModelLaunchOption(nil), options.Models...)
	sort.Slice(models, func(i, j int) bool { return models[i].ModelType < models[j].ModelType })
	workers := append([]manager.WorkerLaunchOption(nil), options.Workers...)
	sort.Slice(workers, func(i, j int) bool { return workers[i].WorkerType < workers[j].WorkerType })
	repositories := append([]manager.RepositoryLaunchOption(nil), options.Repositories...)
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].Slug < repositories[j].Slug })

	modal := sessionModalView{
		Enabled:      true,
		Repositories: make([]repositoryOptionView, 0, len(repositories)),
		RootModels:   newModelOptionViews(models, options.Root.ModelType),
		RootThinking: thinkingOptionsFor(models, options.Root.ModelType, options.Root.ThinkingLevel),
		Workers:      make([]workerOptionView, 0, len(workers)),
	}
	for _, repository := range repositories {
		modal.Repositories = append(modal.Repositories, repositoryOptionView{Slug: repository.Slug})
	}
	for i, worker := range workers {
		modal.Workers = append(modal.Workers, workerOptionView{
			Index:       i,
			Role:        worker.WorkerType,
			Description: worker.Description,
			Models:      newModelOptionViews(models, worker.ModelType),
			Thinking:    thinkingOptionsFor(models, worker.ModelType, worker.ThinkingLevel),
		})
	}
	return indexView{SessionModal: modal}
}

func newModelOptionViews(models []manager.ModelLaunchOption, selected string) []modelOptionView {
	views := make([]modelOptionView, 0, len(models))
	for _, model := range models {
		views = append(views, modelOptionView{
			ID:                   model.ModelType,
			Label:                model.Label,
			ThinkingLevelsCSV:    strings.Join(model.ThinkingLevels, ","),
			DefaultThinkingLevel: model.DefaultThinkingLevel,
			Selected:             model.ModelType == selected,
		})
	}
	return views
}

func thinkingOptionsFor(models []manager.ModelLaunchOption, selectedModel, selectedThinking string) []thinkingOptionView {
	for _, model := range models {
		if model.ModelType != selectedModel {
			continue
		}
		if selectedThinking == "" {
			selectedThinking = model.DefaultThinkingLevel
		}
		levels := make([]thinkingOptionView, 0, len(model.ThinkingLevels))
		for _, level := range model.ThinkingLevels {
			levels = append(levels, thinkingOptionView{Level: level, Selected: level == selectedThinking})
		}
		return levels
	}
	return nil
}

// fleetView is the template data for fleet.html.
type fleetView struct {
	Roots  []rootView
	Issues []issueView
}

// rootView is the template data for one admitted root.
type rootView struct {
	RootSessionID   string
	DisplayName     string
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
//
// Answerable is false when the question's ID failed the manager's safe-charset
// check. A non-answerable question renders its prompt text (still HTML-escaped)
// but no action controls, so a hostile/compromised supervisor cannot smuggle a
// malicious ID into a browser URL, attribute, or JS expression. When Answerable
// is false, ID is cleared to a safe empty string.
type questionSummaryView struct {
	ID          string
	Answerable  bool
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
	RootName        string
	RootDisplayName string
	DisplayLabel    string
	IsRoot          bool
	RootStale       bool
	StreamConnected bool
	Incomplete      bool
	GapText         string
	Provider        string
	Model           string
	ThinkingLevel   string
	Stats           statsView
	CanSteer        bool
	CanInterrupt    bool
	CanStop         bool
}

// actionCapabilities is the server-authoritative projection of which session
// actions are valid for the current lifecycle.
type actionCapabilities struct {
	CanSteer     bool
	CanInterrupt bool
	CanStop      bool
}

// questionPanelView is the template data for questions.html.
type questionPanelView struct {
	SessionID string
	Questions []questionSummaryView
}

// activityItemView is the safe projection of one activity item.
//
// IsMarkdown marks only conversation text (assistant updates and user
// messages) so the browser renders it as safe Markdown. Error/tool text is
// never flagged and stays plain escaped text. The server never renders the
// Markdown itself; it only adds the marker that app.js hands to the sandboxed
// renderer after escaping.
//
// Tool items (IsTool) carry the manager's bounded display fields and a few
// precomputed presentation helpers (card class, running flag, status label).
// The tool args/output are plain strings that html/template escapes in the
// <pre><code> blocks; the browser highlights them via
// KanediasMarkdown.highlight.
type activityItemView struct {
	Seq        uint64
	Kind       string
	Label      string
	Text       string
	ToolName   string
	IsError    bool
	IsMarkdown bool
	// Complete allows the template to leave finalized browser-rendered content
	// untouched while the surrounding activity panel continues to morph.
	Complete bool
	// Tool projection fields (bounded at 64 KiB by the manager).
	IsTool        bool
	ToolSummary   string
	ToolArgs      string
	ToolOutput    string
	ToolLanguage  string
	ToolTruncated bool
	// Per-field truncation markers so the explicit badge stays on the field that
	// was actually cut while ToolTruncated drives the neutral summary indicator.
	ToolArgsTruncated   bool
	ToolOutputTruncated bool
	// Precomputed presentation helpers for the tool card. Running state is kept
	// only in ToolCardClass/StatusLabel; cards are never emitted open.
	ToolCardClass string
	StatusLabel   string
}

// activityUsesMarkdown reports whether an activity kind should be rendered as
// Markdown. Only assistant message updates and user messages qualify; errors
// and tool activity remain plain escaped text.
func activityUsesMarkdown(kind string) bool {
	return kind == "message_update" || kind == "user_message"
}

// activityView is the template data for activity.html.
type activityView struct {
	SessionID  string
	Items      []activityItemView
	Incomplete bool
	GapText    string
}

// deckStatusView is the template data for deck-status.html.
type deckStatusView struct {
	Error   string
	Success bool
	// SuccessID is a per-acknowledgment marker rendered only on the transient
	// success span. Datastar morphs identical DOM, so the client needs a fresh
	// marker to distinguish every "Command sent." acknowledgment and restart
	// its 2000ms auto-clear lifetime.
	SuccessID string
}

// contextView holds nullable context metrics for the Astrolabe dial.
type contextView struct {
	HasPercent bool
	Percent    float64
	// PercentText is the human display of Percent, or "—" when unavailable.
	PercentText string
	// DialDegrees maps Percent (0..100) onto the alidade rotation (0..360°).
	// It is 0 when Percent is unavailable.
	DialDegrees float64
	HasTokens   bool
	Tokens      float64
}

// statsView is the template data for session stats.
type statsView struct {
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

func displayRootName(name, rootID string) string {
	if name != "" {
		return name
	}
	return rootID
}

func sessionDisplayLabel(state manager.SessionState) string {
	if state.Node.SessionID == state.RootSessionID {
		return displayRootName(state.RootName, state.RootSessionID)
	}
	if state.Node.WorkerType != "" {
		return state.Node.WorkerType
	}
	return state.Node.SessionID
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
		DisplayName:     displayRootName(r.Name, r.RootSessionID),
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
	// Constrain the untrusted supervisor-supplied ID at the data boundary. An ID
	// that does not match the safe charset is neutered: the question is marked
	// non-answerable and its ID is dropped so it can never reach a browser URL,
	// DOM attribute, or JS expression. This is the load-bearing XSS defense.
	answerable := manager.ValidQuestionID(q.ID)
	id := q.ID
	if !answerable {
		id = ""
	}
	return questionSummaryView{
		ID:          id,
		Answerable:  answerable,
		Method:      q.Method,
		Title:       q.Title,
		Options:     q.Options,
		Message:     q.Message,
		Placeholder: q.Placeholder,
		Prefill:     q.Prefill,
	}
}

// newActionCapabilities derives action availability from the current session
// state. Stream connectivity is deliberately not an authorization signal.
func newActionCapabilities(state manager.SessionState) actionCapabilities {
	lifecycle := supervisor.LifecycleState(state.Node.Lifecycle)
	canSteer := !state.RootStale && (lifecycle == supervisor.LifecycleReady ||
		lifecycle == supervisor.LifecycleRunning || lifecycle == supervisor.LifecycleAwaitingHandoff)
	return actionCapabilities{
		CanSteer:     canSteer,
		CanInterrupt: !state.RootStale && lifecycle == supervisor.LifecycleRunning,
		CanStop: state.Node.Lifecycle != "" && lifecycle != supervisor.LifecycleStopping &&
			lifecycle != supervisor.LifecycleStopped,
	}
}

// newDetailView converts a SessionState and (optional) stats into the detail
// template data. A zero statsView (HasStats false) renders metrics as "—".
func newDetailView(state manager.SessionState, stats statsView) detailView {
	gapText := ""
	if state.Gap != nil {
		gapText = fmt.Sprintf("replay gap: expected seq %d, first available %d",
			state.Gap.ExpectedSeq, state.Gap.FirstAvailableSeq)
	}
	capabilities := newActionCapabilities(state)
	return detailView{
		SessionID:       state.Node.SessionID,
		WorkerType:      state.Node.WorkerType,
		Lifecycle:       state.Node.Lifecycle,
		RootSessionID:   state.RootSessionID,
		RootName:        state.RootName,
		RootDisplayName: displayRootName(state.RootName, state.RootSessionID),
		DisplayLabel:    sessionDisplayLabel(state),
		IsRoot:          state.Node.SessionID != "" && state.Node.SessionID == state.RootSessionID,
		RootStale:       state.RootStale,
		StreamConnected: state.StreamConnected,
		Incomplete:      state.Incomplete,
		GapText:         gapText,
		Provider:        state.Node.Model.Provider,
		Model:           state.Node.Model.Model,
		ThinkingLevel:   state.Node.Model.ThinkingLevel,
		Stats:           stats,
		CanSteer:        capabilities.CanSteer,
		CanInterrupt:    capabilities.CanInterrupt,
		CanStop:         capabilities.CanStop,
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

// toolCardClass maps a manager tool status onto the compact card's border
// class: pending (recently started), running, done, or error.
func toolCardClass(status string, isError bool) string {
	if isError {
		return "tool-error"
	}
	switch status {
	case "running":
		return "tool-running"
	case "done", "":
		return "tool-done"
	default:
		return "tool-pending"
	}
}

// toolStatusLabel is the short uppercase status text shown in the card summary.
func toolStatusLabel(status string, isError bool) string {
	if isError {
		return "error"
	}
	switch status {
	case "running":
		return "running"
	case "done":
		return "done"
	default:
		return "pending"
	}
}

// newActivityView converts a SessionState into the activity panel data.
func newActivityView(state manager.SessionState) activityView {
	items := make([]activityItemView, 0, len(state.RecentActivity))
	for _, a := range state.RecentActivity {
		view := activityItemView{
			Seq:        a.Seq,
			Kind:       a.Kind,
			Label:      a.Label,
			Text:       a.Text,
			ToolName:   a.ToolName,
			IsError:    a.IsError,
			IsMarkdown: activityUsesMarkdown(a.Kind),
			Complete:   a.Complete,
		}
		if a.IsTool {
			view.IsTool = true
			view.ToolSummary = a.ToolSummary
			view.ToolArgs = a.ToolArgs
			view.ToolOutput = a.ToolOutput
			view.ToolLanguage = a.ToolLanguage
			view.ToolTruncated = a.ToolTruncated
			view.ToolArgsTruncated = a.ToolArgsTruncated
			view.ToolOutputTruncated = a.ToolOutputTruncated
			view.ToolCardClass = toolCardClass(a.Status, a.IsError)
			view.StatusLabel = toolStatusLabel(a.Status, a.IsError)
		}
		items = append(items, view)
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

// newStatsView converts SessionStats into the stats view, computing the
// Astrolabe dial display for the nullable context percentage.
func newStatsView(stats manager.SessionStats) statsView {
	ctx := contextView{PercentText: "—"}
	if stats.ContextUsage != nil {
		if stats.ContextUsage.Percent != nil {
			p := *stats.ContextUsage.Percent
			ctx.HasPercent = true
			ctx.Percent = p
			ctx.PercentText = fmt.Sprintf("%.0f%%", p)
			// Clamp to [0,100] before mapping onto the 0..360° alidade sweep.
			clamped := p
			if clamped < 0 {
				clamped = 0
			}
			if clamped > 100 {
				clamped = 100
			}
			ctx.DialDegrees = clamped * 3.6
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

// deckStatusCounter serializes per-success markers so two rapid
// acknowledgments never collide even when the browser sees identical DOM.
var deckStatusCounter atomic.Uint64

// genDeckSuccessID returns a fresh, monotonically increasing success marker.
func genDeckSuccessID() string {
	return "ack-" + strconv.FormatUint(deckStatusCounter.Add(1), 10)
}

// newDeckStatusView converts an operation error into a deck status view.
func newDeckStatusView(err error) deckStatusView {
	if err == nil {
		return deckStatusView{Success: true, SuccessID: genDeckSuccessID()}
	}
	return deckStatusView{Error: operatorMessage(err)}
}

// operatorMessage converts an internal error to a safe operator-facing message.
// Typed contract errors map to stable copy; all others produce a generic message.
// Never includes socket paths, session files, capabilities, or panic values.
func operatorMessage(err error) string {
	if err == nil {
		return ""
	}
	var ce *contract.Error
	if errors.As(err, &ce) {
		switch ce.Code {
		case contract.ErrorSessionStopping:
			return "The session is already stopping."
		case contract.ErrorNotFound:
			return "The session could not be found."
		case contract.ErrorSaturated:
			return "The system is at capacity. Try again shortly."
		case contract.ErrorConflict:
			return "The command could not be applied to the current session state."
		case contract.ErrorInvalidRequest:
			return "The command was not valid."
		case contract.ErrorUnknownWorkerType:
			return "The session worker type is not supported."
		case contract.ErrorForbiddenRPC:
			return "The command is not permitted for this session."
		case contract.ErrorProxyUnavailable, contract.ErrorChildUnavailable:
			return "The session is temporarily unavailable."
		case contract.ErrorProvisioningFailed:
			return "The session could not be started."
		case contract.ErrorChildFailed, contract.ErrorChildAborted:
			return "The session ended unexpectedly."
		case contract.ErrorHandoffRefMissing, contract.ErrorHandoffRefMismatch:
			return "The session handoff could not be completed."
		}
	}
	return "The supervisor command could not be completed."
}
