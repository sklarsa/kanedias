package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisorapi"
)

// errNotFound is returned when a requested session/route does not exist; the
// server maps it to an HTTP 404.
var errNotFound = errors.New("manager: not found")

// Manager is the root-only control plane between the recursive supervisor
// API and the web server.
type Manager struct {
	mu              sync.Mutex
	opts            Options
	roots           map[string]*rootHandle // keyed by socket path
	routes          map[string]string      // sessionID -> rootID
	discoveryIssues []DiscoveryIssue
	factory         clientFactory
	starter         processStarter
	// Root-bootstrap write/join seams retain production behavior while making
	// blocked-write lifecycle ownership deterministic in tests.
	newSpawnToken          func() (string, error)
	newBootstrapPipe       func() (*os.File, *os.File, error)
	writeRootBootstrap     func(io.Writer, []byte) error
	waitRootBootstrapWrite func(<-chan struct{})
	closed                 bool // set once Close begins; blocks new monitor loops
	clientsClosed          bool // set once clients have been closed (idempotency)
	quiesced               bool
	// monitoring infrastructure
	closeCtx        context.Context
	closeCancel     context.CancelFunc
	snapshotCtx     context.Context
	snapshotCancel  context.CancelFunc
	monitorWG       sync.WaitGroup
	fleetFanout     *changeFanout
	sessionFanout   *changeFanout
	fleetRevision   uint64
	sessionRevision uint64
	// afterCommitSpawnHook, if non-nil, is invoked inside commitSpawn between
	// commitTree and monitorRoot. Test-only seam for the MGR-D interleaving; nil
	// in production.
	afterCommitSpawnHook func(committed *rootHandle)

	// launch is the immutable allowlisted launch catalog resolved from Options.
	launch LaunchConfiguration
}

// New normalizes and validates options, resolves defaults, and creates the
// required directories.
func New(opts Options) (*Manager, error) {
	if opts.EventLimits.MaxEvents <= 0 && opts.EventLimits.MaxBytes <= 0 {
		return nil, errors.New("manager: event limits require at least one positive bound")
	}
	if opts.Logger == nil {
		return nil, errors.New("manager: logger is required")
	}
	if len(opts.Launch.modelOrder) == 0 {
		return nil, errors.New("manager: launch configuration is required")
	}

	// Resolve RootSocketDir default.
	if opts.RootSocketDir == "" {
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			opts.RootSocketDir = filepath.Join(xdg, "kanedias", "roots")
		} else {
			opts.RootSocketDir = fmt.Sprintf("/tmp/kanedias-%d/roots", os.Geteuid())
		}
	}
	// Resolve SessionLogDir default.
	if opts.SessionLogDir == "" {
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			opts.SessionLogDir = filepath.Join(xdg, "kanedias", "sessions")
		} else {
			home, err := userHomeDir()
			if err != nil {
				return nil, fmt.Errorf("manager: resolve session log dir: %w", err)
			}
			opts.SessionLogDir = filepath.Join(home, ".local", "state", "kanedias", "sessions")
		}
	}

	// Clean paths.
	opts.RootSocketDir = filepath.Clean(opts.RootSocketDir)
	opts.SessionLogDir = filepath.Clean(opts.SessionLogDir)

	if !filepath.IsAbs(opts.RootSocketDir) {
		return nil, fmt.Errorf("manager: root socket dir %q must be absolute", opts.RootSocketDir)
	}
	if !filepath.IsAbs(opts.SessionLogDir) {
		return nil, fmt.Errorf("manager: session log dir %q must be absolute", opts.SessionLogDir)
	}

	// Validate or create the final socket directory.
	if err := ensurePrivateDir(opts.RootSocketDir); err != nil {
		return nil, fmt.Errorf("manager: root socket dir: %w", err)
	}
	if err := ensurePrivateDir(opts.SessionLogDir); err != nil {
		return nil, fmt.Errorf("manager: session log dir: %w", err)
	}

	// Validate unix path length for a plausible socket file inside RootSocketDir.
	// The real spawn token is 32 hex chars (16 random bytes = 128 bits of
	// entropy), so the probe uses the same length to accurately reflect the real
	// filename. Keep this in sync with generateToken / spawnTokenHexLen.
	testPath := filepath.Join(opts.RootSocketDir, strings.Repeat("a", spawnTokenHexLen)+".root.sock")
	if err := validateUnixPathLength(testPath); err != nil {
		return nil, fmt.Errorf("manager: root socket dir path too long: %w", err)
	}

	// Validate ConfigPath.
	if opts.ConfigPath != "" {
		clean := filepath.Clean(opts.ConfigPath)
		if !filepath.IsAbs(clean) {
			return nil, fmt.Errorf("manager: config path %q must be absolute", opts.ConfigPath)
		}
		opts.ConfigPath = clean
	}

	// Resolve SessionBinary to the current executable by default so the normal
	// configuration can spawn independent roots without an explicit override.
	if opts.SessionBinary == "" {
		binary, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("manager: resolve current executable: %w", err)
		}
		opts.SessionBinary = binary
	}
	resolvedBinary, err := resolveExecutable(opts.SessionBinary)
	if err != nil {
		return nil, fmt.Errorf("manager: session binary: %w", err)
	}
	opts.SessionBinary = resolvedBinary

	// Apply default intervals.
	if opts.DiscoveryInterval == 0 {
		opts.DiscoveryInterval = defaultDiscoveryInterval
	}
	if opts.SnapshotInterval == 0 {
		opts.SnapshotInterval = defaultSnapshotInterval
	}
	if opts.SpawnTimeout == 0 {
		opts.SpawnTimeout = defaultSpawnTimeout
	}
	if opts.DiscoveryInterval < 0 || opts.SnapshotInterval < 0 || opts.SpawnTimeout < 0 {
		return nil, errors.New("manager: intervals must be positive")
	}

	ctx, cancel := context.WithCancel(context.Background())
	snapshotCtx, snapshotCancel := context.WithCancel(ctx)
	m := &Manager{
		opts:                   opts,
		roots:                  make(map[string]*rootHandle),
		routes:                 make(map[string]string),
		factory:                defaultClientFactory,
		starter:                osProcessStarter{},
		newSpawnToken:          generateToken,
		newBootstrapPipe:       os.Pipe,
		writeRootBootstrap:     writeRootBootstrap,
		waitRootBootstrapWrite: waitRootBootstrapWrite,
		closeCtx:               ctx,
		closeCancel:            cancel,
		snapshotCtx:            snapshotCtx,
		snapshotCancel:         snapshotCancel,
		fleetFanout:            newChangeFanout(supervisor.DefaultSubscriberMailboxCapacity),
		sessionFanout:          newChangeFanout(supervisor.DefaultSubscriberMailboxCapacity),
		launch:                 opts.Launch,
	}
	return m, nil
}

// LaunchOptions returns the manager's read-only launch view for the server to
// render. The returned value carries copied slices and is safe to retain.
func (m *Manager) LaunchOptions() SessionLaunchOptions {
	return m.launch.LaunchOptions()
}

func defaultClientFactory(socketPath string) (rootClient, error) {
	return supervisorapi.NewClient(socketPath)
}

// ensurePrivateDir creates the directory if absent, then validates it.
func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return validatePrivateDir(path)
}

// validateUnixPathLength checks that the path fits within the platform's
// UNIX_PATH_MAX (108 bytes on Linux).
func validateUnixPathLength(path string) error {
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	if len(addr.Name) > 107 {
		return fmt.Errorf("unix path %q exceeds maximum length (107 bytes)", path)
	}
	return nil
}

// resolveExecutable returns a clean absolute path to a regular executable.
// The target of a symlink may itself be a regular file.
func resolveExecutable(path string) (string, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("%q is not an absolute path", path)
	}
	info, err := os.Stat(clean) // follows symlinks
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", clean)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%q is not executable", clean)
	}
	return clean, nil
}

func userHomeDir() (string, error) {
	if home := os.Getenv("HOME"); home != "" {
		return home, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// Start performs one discovery pass and launches periodic discovery and
// monitoring goroutines for all admitted roots.
func (m *Manager) Start(ctx context.Context) error {
	m.discoverOnce(ctx)
	m.mu.Lock()
	handles := make([]*rootHandle, 0, len(m.roots))
	for _, h := range m.roots {
		handles = append(handles, h)
	}
	m.mu.Unlock()
	for _, h := range handles {
		m.monitorRoot(h)
	}
	go m.discoveryLoop()
	return nil
}

// discoveryLoop runs periodic discovery until the manager is closed.
func (m *Manager) discoveryLoop() {
	ticker := time.NewTicker(m.opts.DiscoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.snapshotCtx.Done():
			return
		case <-ticker.C:
			m.discoverOnce(m.snapshotCtx)
		}
	}
}

// Fleet returns the current fleet projection.
func (m *Manager) Fleet() FleetSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	roots := make([]RootState, 0, len(m.roots))
	for _, handle := range m.roots {
		var gap *ReplayGap
		incomplete := false
		if handle.mirror != nil {
			gap = handle.mirror.Gap()
			incomplete = gap != nil
		}
		roots = append(roots, RootState{
			RootSessionID:   handle.rootID,
			Tree:            handle.tree,
			Stale:           handle.stale,
			StreamConnected: handle.streamConnected,
			Incomplete:      incomplete,
			Gap:             gap,
			Revision:        m.fleetRevision,
		})
	}
	issues := append([]DiscoveryIssue(nil), m.discoveryIssues...)
	// Sort roots deterministically (map iteration is unordered) so the fleet does
	// not reshuffle between SSE re-renders.
	sort.Slice(roots, func(i, j int) bool { return roots[i].RootSessionID < roots[j].RootSessionID })
	return FleetSnapshot{Roots: roots, Issues: issues, Revision: m.fleetRevision}
}

// Session returns the projection for one session in an admitted root tree.
func (m *Manager) Session(sessionID string) (SessionState, error) {
	m.mu.Lock()
	rootID, ok := m.routes[sessionID]
	if !ok {
		m.mu.Unlock()
		return SessionState{}, errNotFound
	}
	var handle *rootHandle
	for _, h := range m.roots {
		if h.rootID == rootID {
			handle = h
			break
		}
	}
	if handle == nil {
		m.mu.Unlock()
		return SessionState{}, errNotFound
	}
	node, found := findNode(handle.tree, sessionID)
	if !found {
		m.mu.Unlock()
		return SessionState{}, errNotFound
	}
	var events []supervisor.EventEnvelope
	var gap *ReplayGap
	incomplete := false
	if handle.mirror != nil {
		events = handle.mirror.EventsFor(sessionID)
		gap = handle.mirror.Gap()
		incomplete = gap != nil
	}
	state := SessionState{
		RootSessionID:   rootID,
		Node:            node,
		RootStale:       handle.stale,
		StreamConnected: handle.streamConnected,
		Incomplete:      incomplete,
		Gap:             gap,
		RecentActivity:  projectActivity(events, sessionID),
		Revision:        m.sessionRevision,
	}
	m.mu.Unlock()
	return state, nil
}

func findNode(snapshot supervisor.NodeSnapshot, sessionID string) (supervisor.NodeSnapshot, bool) {
	if snapshot.SessionID == sessionID {
		return snapshot, true
	}
	for _, child := range snapshot.Children {
		if found, ok := findNode(child, sessionID); ok {
			return found, true
		}
	}
	return supervisor.NodeSnapshot{}, false
}

// SubscribeFleet registers a bounded fleet change subscriber.
func (m *Manager) SubscribeFleet() ChangeSubscription {
	return m.fleetFanout.Subscribe()
}

// SubscribeSession registers a bounded subscriber for one session.
func (m *Manager) SubscribeSession(sessionID string) (ChangeSubscription, error) {
	m.mu.Lock()
	_, ok := m.routes[sessionID]
	m.mu.Unlock()
	if !ok {
		return ChangeSubscription{}, errNotFound
	}
	return m.sessionFanout.Subscribe(), nil
}

// Quiesce rejects new writes and stops discovery/snapshot polling while event
// drains continue until Close.
func (m *Manager) Quiesce(context.Context) error {
	m.mu.Lock()
	if !m.quiesced {
		m.quiesced = true
		m.snapshotCancel()
	}
	m.mu.Unlock()
	return nil
}

// Close cancels subscriptions, waits for manager monitor goroutines, and
// closes clients without stopping admitted roots.
func (m *Manager) Close(ctx context.Context) error {
	_ = m.Quiesce(ctx)
	// Mark closed under m.mu before waiting so that any concurrent monitorRoot
	// either completes its WaitGroup.Add before this point (and is waited on) or
	// observes closed==true and refuses to Add. This makes Add happen-before
	// Wait per the sync.WaitGroup contract.
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.closeCancel()
	done := make(chan struct{})
	go func() { m.monitorWG.Wait(); close(done) }()
	select {
	case <-done:
		return m.closeClients()
	case <-ctx.Done():
		// MGR-E (accepted tradeoff): if the caller's ctx expires before the
		// monitor loops drain, the Wait goroutine above and the roots' clients are
		// left open. We intentionally do NOT close the clients here: a loop that is
		// wedged in an in-flight call still holds a client reference, so closing it
		// from under the loop would be a use-after-close (the very class of bug
		// MGR-A fixes). closeClients remains idempotent, so a subsequent Close with
		// a longer deadline (after the loops finally exit) cleans up correctly.
		// In practice per-root cancellation (MGR-B) plus context-aware client
		// calls make an indefinitely-wedged loop unreachable, so this window is a
		// deadline-exceeded edge, not a steady-state leak.
		return ctx.Err()
	}
}

func (m *Manager) closeClients() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clientsClosed {
		return nil
	}
	m.clientsClosed = true
	m.fleetFanout.Close()
	m.sessionFanout.Close()
	for socketPath, handle := range m.roots {
		_ = handle.client.Close()
		delete(m.roots, socketPath)
	}
	// MGR-F: clear routes too so post-Close state is consistent (no ghost routes
	// pointing at now-removed roots).
	m.routes = make(map[string]string)
	return nil
}

// Default interval constants.
const (
	defaultDiscoveryInterval = 5e9   // 5s
	defaultSnapshotInterval  = 10e9  // 10s
	defaultSpawnTimeout      = 120e9 // 2m
)
