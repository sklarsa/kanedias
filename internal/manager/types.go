package manager

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

// Options configures a Manager. Paths and binaries are resolved and secured
// by New; zero intervals fall back to validated defaults.
type Options struct {
	ConfigPath        string
	RootSocketDir     string
	SessionLogDir     string
	SessionBinary     string
	DiscoveryInterval time.Duration
	SnapshotInterval  time.Duration
	SpawnTimeout      time.Duration
	EventLimits       supervisor.EventBrokerOptions
	Launch            LaunchConfiguration
	Logger            *slog.Logger
}

// ReplayGap records the first missing range of a root's replay stream.
type ReplayGap struct {
	ExpectedSeq       uint64
	FirstAvailableSeq uint64
}

// DiscoveryIssue describes a sanitized malformed or conflicting root
// candidate seen during a scan.
type DiscoveryIssue struct {
	SocketName string
	Code       string
	Message    string
}

// RootState is the public projection of one admitted root tree.
type RootState struct {
	RootSessionID   string
	Name            string
	Tree            supervisor.NodeSnapshot
	Stale           bool
	StreamConnected bool
	Incomplete      bool
	Gap             *ReplayGap
	Revision        uint64
}

// maxToolDisplayBytes caps any single tool arguments/output display field. It
// bounds the bytes the manager projects from a supervisor's raw tool payloads
// so an untrusted/hostile supervisor can never make the manager emit an
// unbounded string into the transcript. Display truncation appends the marker
// byte suffix, so the effective text budget is maxToolDisplayBytes across the
// whole field.
const maxToolDisplayBytes = 64 << 10

// ActivityItem is one allowlisted projection of recent session activity.
type ActivityItem struct {
	Seq        uint64
	Kind       string
	Label      string
	Text       string
	ImageCount int
	ToolCallID string
	ToolName   string
	Status     string
	IsError    bool
	// IsTool marks bounded tool-execution projections. The tool display fields
	// below are the manager's sole raw-event trust boundary: they are derived
	// here from the supervisor's raw payload via pure bolding/formatting
	// helpers (boundedDisplay, formatToolJSON, formatToolResult, summarizeTool,
	// toolLanguage) and never carry template.HTML or raw event data.
	IsTool       bool
	ToolSummary  string
	ToolArgs     string
	ToolOutput   string
	ToolLanguage string
	// ToolTruncated is the aggregate flag across arguments, partial and final
	// output; the card summary surfaces a neutral "truncated" indicator from it.
	ToolTruncated bool
	// ToolArgsTruncated / ToolOutputTruncated mark which specific field was
	// actually cut, so the explicit marker stays on the affected field.
	ToolArgsTruncated   bool
	ToolOutputTruncated bool
	// Complete reports that later events cannot change this item's displayed
	// source content. The server uses it only to protect browser-rendered DOM.
	Complete bool
}

// SessionState is the public projection of one session within a root tree.
type SessionState struct {
	RootSessionID   string
	RootName        string
	Node            supervisor.NodeSnapshot
	RootStale       bool
	StreamConnected bool
	Incomplete      bool
	Gap             *ReplayGap
	RecentActivity  []ActivityItem
	Revision        uint64
}

// FleetSnapshot is a consistent point-in-time view of every admitted root.
type FleetSnapshot struct {
	Roots    []RootState
	Issues   []DiscoveryIssue
	Revision uint64
}

// ChangeSubscription delivers monotonically increasing revisions. Updates is
// closed when the subscription is disconnected; Close is idempotent.
type ChangeSubscription struct {
	Updates <-chan uint64
	Close   func()
}

// SessionStats are supported typed metrics for one session.
type SessionStats struct {
	UserMessages      int
	AssistantMessages int
	ToolCalls         int
	ToolResults       int
	TotalMessages     int
	Tokens            TokenStats
	Cost              float64
	ContextUsage      *ContextUsage
}

// TokenStats groups token counters returned by Pi.
type TokenStats struct {
	Input, Output, CacheRead, CacheWrite, Total int64
}

// ContextUsage reports nullable compaction/context state.
type ContextUsage struct {
	Tokens        *float64
	ContextWindow float64
	Percent       *float64
}

// rootClient is the manager-private client seam over the supervisor routed
// API so tests do not widen the supervisor's parent/child interface.
type rootClient interface {
	Snapshot(ctx context.Context) (supervisor.NodeSnapshot, error)
	Subscribe(ctx context.Context) (supervisor.Subscription, error)
	CallRPC(ctx context.Context, sessionID string, payload json.RawMessage) (json.RawMessage, error)
	AnswerQuestion(ctx context.Context, sessionID string, questionID string, answer json.RawMessage) error
	Stop(ctx context.Context, sessionID string) error
	Close() error
}

type clientFactory func(socketPath string) (rootClient, error)
