package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
)

// spawnSpec describes a process to be started.
type spawnSpec struct {
	Path        string
	Args        []string
	Env         []string
	Stdin       *os.File
	Output      *os.File
	ExtraFiles  []*os.File
	SysProcAttr *syscall.SysProcAttr
}

// spawnedProcess abstracts a running OS process for testing.
type spawnedProcess interface {
	PID() int
	Done() <-chan struct{}
	WaitErr() error
	SignalGroup(syscall.Signal) error
}

// processStarter starts a process from a spawnSpec.
type processStarter interface {
	Start(spawnSpec) (spawnedProcess, error)
}

func writeRootBootstrap(writer io.Writer, encoded []byte) error {
	_, err := writer.Write(encoded)
	return err
}

func waitRootBootstrapWrite(done <-chan struct{}) {
	<-done
}

const defaultRootAbortWait = 5 * time.Second

// observeRootBootstrapAbort preserves the single Wait owner and boundedly
// observes its cached completion after aborting a root whose bootstrap could
// not be delivered. The observation deliberately ignores the request context:
// cancellation must not allow an unobserved process to escape this abort path.
func (m *Manager) observeRootBootstrapAbort(spawned spawnedProcess, primary error) error {
	signalErr := spawned.SignalGroup(syscall.SIGKILL)
	wait := m.rootAbortWait
	if wait <= 0 {
		wait = defaultRootAbortWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-spawned.Done():
		var exitErr error
		if err := spawned.WaitErr(); err != nil {
			exitErr = fmt.Errorf("manager: root exit after bootstrap abort: %w", err)
		}
		return errors.Join(primary, wrapRootAbortSignalError(signalErr), exitErr)
	case <-timer.C:
		return errors.Join(primary, wrapRootAbortSignalError(signalErr), fmt.Errorf("manager: timed out after %s observing root bootstrap abort", wait))
	}
}

func wrapRootAbortSignalError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("manager: signal root after bootstrap abort: %w", err)
}

// startedProcess wraps exec.Cmd and caches the Wait result.
type startedProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
}

func (p *startedProcess) PID() int              { return p.cmd.Process.Pid }
func (p *startedProcess) Done() <-chan struct{} { return p.done }
func (p *startedProcess) WaitErr() error        { return p.waitErr }
func (p *startedProcess) SignalGroup(sig syscall.Signal) error {
	if p.cmd.Process == nil {
		return nil
	}
	select {
	case <-p.done:
		return nil // process already exited
	default:
	}
	return syscall.Kill(-p.cmd.Process.Pid, sig)
}

func (p *startedProcess) waitExactlyOnce() {
	p.waitErr = p.cmd.Wait()
	close(p.done)
}

// osProcessStarter is the production processStarter.
type osProcessStarter struct{}

func (osProcessStarter) Start(spec spawnSpec) (spawnedProcess, error) {
	cmd := exec.Command(spec.Path, spec.Args[1:]...)
	cmd.Env = spec.Env
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Output
	cmd.Stderr = spec.Output
	cmd.ExtraFiles = spec.ExtraFiles
	cmd.SysProcAttr = spec.SysProcAttr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	started := &startedProcess{cmd: cmd, done: make(chan struct{})}
	go started.waitExactlyOnce()
	return started, nil
}

type rootStatusResult struct {
	status process.RootStartupStatus
	err    error
}

// pendingRoot tracks a root that is awaiting admission.
type pendingRoot struct {
	socketPath   string
	identity     socketIdentity
	logPath      string
	process      spawnedProcess
	client       rootClient
	rootID       string // set after first successful snapshot
	name         string // normalized optional display name, resolved before spawn
	statusRead   *os.File
	statusResult <-chan rootStatusResult
	statusOnce   sync.Once
}

func (pending *pendingRoot) closeRootStatus() {
	pending.statusOnce.Do(func() {
		if pending.statusRead != nil {
			_ = pending.statusRead.Close()
		}
	})
}

func (pending *pendingRoot) startRootStatusDecoder() {
	result := make(chan rootStatusResult, 1)
	pending.statusResult = result
	go func() {
		status, err := process.DecodeRootStartupStatus(pending.statusRead)
		pending.closeRootStatus()
		result <- rootStatusResult{status: status, err: err}
	}()
}

// probe fetches a snapshot from the pending root and validates the socket
// identity. Returns the snapshot, the post-probe identity, and any error.
func (pending *pendingRoot) probe(ctx context.Context, factory clientFactory) (supervisor.NodeSnapshot, socketIdentity, error) {
	if pending.client == nil {
		client, err := factory(pending.socketPath)
		if err != nil {
			return supervisor.NodeSnapshot{}, socketIdentity{}, err
		}
		pending.client = client
	}
	snapshot, err := pending.client.Snapshot(ctx)
	if err != nil {
		_ = pending.client.Close()
		pending.client = nil
		return supervisor.NodeSnapshot{}, socketIdentity{}, err
	}
	postID, err := inspectRootSocket(pending.socketPath, os.Lstat, os.Geteuid())
	if err != nil || postID != pending.identity {
		_ = pending.client.Close()
		pending.client = nil
		return supervisor.NodeSnapshot{}, socketIdentity{}, fmt.Errorf("socket identity changed during probe")
	}
	return snapshot, postID, nil
}

// stopIfResponsive sends Stop to the root if we know its ID.
func (pending *pendingRoot) stopIfResponsive(ctx context.Context) error {
	if pending.rootID == "" || pending.client == nil {
		return nil
	}
	return pending.client.Stop(ctx, pending.rootID)
}

// safeUnlinkOwnedSocket removes the socket file only if it is still the exact
// socket generation observed during admission.
func (pending *pendingRoot) safeUnlinkOwnedSocket() error {
	if !sameIdentity(pending.socketPath, pending.identity) {
		return nil
	}
	return os.Remove(pending.socketPath)
}

// SpawnRoot launches a root with the configured default model policy.
func (m *Manager) SpawnRoot(ctx context.Context) (string, error) {
	return m.SpawnRootWithRequest(ctx, m.launch.DefaultRequest())
}

// SpawnRootWithRequest validates an allowlisted launch request before creating
// any spawn artifact, then transfers the resolved policy through inherited fd 3.
func (m *Manager) SpawnRootWithRequest(ctx context.Context, request SessionLaunchRequest) (string, error) {
	resolved, err := m.launch.Resolve(request)
	if err != nil {
		return "", err
	}
	var encoded bytes.Buffer
	if err := process.EncodeRootBootstrap(&encoded, process.RootBootstrap{Policy: resolved.Policy, Workspace: resolved.Workspace}); err != nil {
		return "", fmt.Errorf("manager: encode root bootstrap: %w", err)
	}

	m.mu.Lock()
	q := m.quiesced
	m.mu.Unlock()
	if q {
		return "", errors.New("manager: quiesced, cannot spawn")
	}
	if m.opts.SessionBinary == "" {
		return "", errors.New("manager: session binary is not configured")
	}
	if m.opts.ConfigPath == "" {
		return "", errors.New("manager: config path is not configured")
	}

	token, err := m.newSpawnToken()
	if err != nil {
		return "", fmt.Errorf("manager: generate spawn token: %w", err)
	}
	socketPath := filepath.Join(m.opts.RootSocketDir, token+".root.sock")
	if err := validateUnixPathLength(socketPath); err != nil {
		return "", fmt.Errorf("manager: socket path: %w", err)
	}

	logPath := filepath.Join(m.opts.SessionLogDir, token+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("manager: create log file: %w", err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		_ = logFile.Close()
		_ = os.Remove(logPath)
		return "", fmt.Errorf("manager: open /dev/null: %w", err)
	}
	bootstrapRead, bootstrapWrite, err := m.newBootstrapPipe()
	if err != nil {
		_ = devNull.Close()
		_ = logFile.Close()
		_ = os.Remove(logPath)
		return "", fmt.Errorf("manager: create root bootstrap pipe: %w", err)
	}
	closeBootstrap := func() {
		_ = bootstrapRead.Close()
		_ = bootstrapWrite.Close()
	}
	newStatusPipe := m.newRootStatusPipe
	if newStatusPipe == nil {
		newStatusPipe = os.Pipe
	}
	statusRead, statusWrite, err := newStatusPipe()
	if err != nil {
		closeBootstrap()
		_ = devNull.Close()
		_ = logFile.Close()
		_ = os.Remove(logPath)
		return "", fmt.Errorf("manager: create root status pipe: %w", err)
	}
	pending := &pendingRoot{socketPath: socketPath, logPath: logPath, name: resolved.Name, statusRead: statusRead}

	spec := spawnSpec{
		Path: m.opts.SessionBinary,
		Args: []string{
			m.opts.SessionBinary,
			"--config", m.opts.ConfigPath,
			"session", "--socket", socketPath,
			"--bootstrap-fd", fmt.Sprint(process.RootBootstrapFD),
			"--status-fd", fmt.Sprint(process.RootStatusFD),
		},
		Env:        os.Environ(),
		Stdin:      devNull,
		Output:     logFile,
		ExtraFiles: []*os.File{bootstrapRead, statusWrite},
		SysProcAttr: &syscall.SysProcAttr{
			Setsid: true,
		},
	}

	spawned, err := m.starter.Start(spec)
	_ = bootstrapRead.Close()
	_ = statusWrite.Close()
	_ = logFile.Close()
	_ = devNull.Close()
	if err != nil {
		_ = bootstrapWrite.Close()
		pending.closeRootStatus()
		_ = os.Remove(logPath)
		return "", fmt.Errorf("manager: start root process: %w", err)
	}
	pending.process = spawned
	pending.startRootStatusDecoder()

	writeDone := make(chan struct{})
	var writeErr error
	go func() {
		defer close(writeDone)
		writeErr = errors.Join(m.writeRootBootstrap(bootstrapWrite, encoded.Bytes()), bootstrapWrite.Close())
	}()
	select {
	case <-writeDone:
		if writeErr != nil {
			closeBootstrap()
			pending.closeRootStatus()
			primary := fmt.Errorf("manager: send root bootstrap: %w", writeErr)
			return "", m.observeRootBootstrapAbort(spawned, primary)
		}
	case <-ctx.Done():
		_ = bootstrapWrite.Close()
		m.waitRootBootstrapWrite(writeDone)
		closeBootstrap()
		pending.closeRootStatus()
		primary := errors.Join(ctx.Err(), writeErr)
		return "", m.observeRootBootstrapAbort(spawned, primary)
	}
	closeBootstrap()

	rootID, err := m.admitRoot(ctx, pending)
	if err != nil {
		go m.cleanupFailedSpawn(pending)
		return "", err
	}
	return rootID, nil
}

// admitRoot polls the pending root until it becomes admissible or gives up.
func (m *Manager) admitRoot(ctx context.Context, pending *pendingRoot) (string, error) {
	defer pending.closeRootStatus()
	spawnTimeout := m.opts.SpawnTimeout
	if spawnTimeout <= 0 {
		spawnTimeout = defaultSpawnTimeout
	}
	admissionCtx, admissionCancel := context.WithTimeout(ctx, spawnTimeout)
	defer admissionCancel()

	probe := time.NewTicker(500 * time.Millisecond)
	defer probe.Stop()

	for {
		select {
		case result := <-pending.statusResult:
			pending.statusResult = nil
			if err := m.rootStatusAdmissionError(result); err != nil {
				return "", err
			}
		case <-pending.process.Done():
			return "", m.rootExitAdmissionError(pending)
		case <-admissionCtx.Done():
			return "", admissionCtx.Err()
		case <-probe.C:
			// Wait for socket to appear.
			if _, err := inspectRootSocket(pending.socketPath, os.Lstat, os.Geteuid()); err != nil {
				continue
			}
			identity, err := inspectRootSocket(pending.socketPath, os.Lstat, os.Geteuid())
			if err != nil {
				continue
			}
			if pending.identity == (socketIdentity{}) {
				pending.identity = identity
			} else if pending.identity != identity {
				return "", fmt.Errorf("socket identity changed during admission")
			}

			snapshot, postIdentity, err := pending.probe(admissionCtx, m.factory)
			if err != nil {
				continue
			}
			pending.identity = postIdentity
			// Record the root's session ID as soon as a valid snapshot is
			// obtained (before the admissible/commit checks) so that a later
			// cleanup can issue a graceful Stop for a responsive-but-unadmitted
			// root instead of jumping straight to signals.
			if snapshot.SessionID != "" {
				pending.rootID = snapshot.SessionID
			}

			lc := supervisor.LifecycleState(snapshot.Lifecycle)
			if lc == supervisor.LifecycleProvisioning || lc == supervisor.LifecycleStarting {
				continue
			}
			if !admissible(snapshot) {
				continue
			}
			// The probe can block while the root exits or its startup decoder
			// reports failure. Observe both again before making the snapshot
			// durable, and drain status whenever process completion is visible.
			if err := m.observeRootBeforeCommit(pending); err != nil {
				return "", err
			}
			// Atomic commit.
			return m.commitSpawn(pending, snapshot)
		}
	}
}

func (m *Manager) observeRootBeforeCommit(pending *pendingRoot) error {
	select {
	case result := <-pending.statusResult:
		pending.statusResult = nil
		if err := m.rootStatusAdmissionError(result); err != nil {
			return err
		}
	default:
	}
	select {
	case <-pending.process.Done():
		return m.rootExitAdmissionError(pending)
	default:
		return nil
	}
}

func (m *Manager) rootExitAdmissionError(pending *pendingRoot) error {
	if pending.statusResult != nil {
		result := <-pending.statusResult
		pending.statusResult = nil
		if err := m.rootStatusAdmissionError(result); err != nil {
			return err
		}
	}
	return fmt.Errorf("root exited before admission: %w", pending.process.WaitErr())
}

func (m *Manager) rootStatusAdmissionError(result rootStatusResult) error {
	if result.err != nil {
		m.opts.Logger.Warn("root startup status decode failed", "err", result.err)
		return errors.New("manager: root startup status unavailable")
	}
	if result.status.Status != process.RootStartupFailure {
		return nil
	}
	message := "root failed to start"
	if result.status.Code == contract.ErrorWorkspaceRepositoryUnavailable {
		message = "selected workspace repository is unavailable"
	}
	return contract.NewError(result.status.Code, message)
}

// commitSpawn validates and atomically commits a pending root into the fleet.
func (m *Manager) commitSpawn(pending *pendingRoot, snapshot supervisor.NodeSnapshot) (string, error) {
	normalized, candidate, err := validateRootTree(snapshot)
	if err != nil {
		return "", fmt.Errorf("spawn admission tree invalid: %w", err)
	}

	handle := &rootHandle{
		socketPath: pending.socketPath,
		rootID:     snapshot.SessionID,
		identity:   pending.identity,
		actionable: true,
		client:     pending.client,
	}
	res, err := m.commitTree(handle, normalized, candidate)
	if err != nil {
		return "", fmt.Errorf("spawn admission route conflict: %w", err)
	}
	// Transfer client ownership — pending.client is now owned by the committed
	// handle (unless commitTree reused an existing handle and closed our client).
	pending.client = nil
	// If a replaced-inode handle was displaced, drain its loops and close its old
	// client outside the lock (MGR-A).
	m.drainAndCloseDisplaced(res.displaced)

	// Test seam (MGR-D): lets a test deterministically inject a concurrent
	// discovery that reuses+starts monitoring res.handle in the window between
	// our commitTree and our monitorRoot. nil in production.
	if m.afterCommitSpawnHook != nil {
		m.afterCommitSpawnHook(res.handle)
	}

	// Start monitoring the newly admitted root.
	//
	// MGR-D: monitorRoot returns a tri-state. It is NOT an error for the handle
	// to already be live — a concurrent discovery may have reused this exact
	// socket and started its loops between our commitTree and this call. In that
	// case the root is admitted and monitored; we must NOT remove it or return a
	// bogus "closing" error. We only tear down when the manager is genuinely
	// closing (monitorRefusedClosing) AND the committed handle is our own fresh
	// handle (never a reused live one).
	switch m.monitorRoot(res.handle) {
	case monitorRefusedClosing:
		if res.handle == handle {
			m.removeRootBySocketPath(handle.socketPath, handle)
		}
		return "", errors.New("manager: closing, cannot admit spawned root")
	case monitorStarted, monitorAlreadyLive:
		// Admitted and live either way.
	}
	m.bumpFleetRevision()
	return snapshot.SessionID, nil
}

// cleanupTimeouts configures the graceful-cleanup escalation budget. Production
// uses defaultCleanupTimeouts; tests inject short durations to keep runs fast.
type cleanupTimeouts struct {
	overall  time.Duration // total budget for the whole escalation
	stop     time.Duration // graceful root Stop attempt
	termWait time.Duration // wait after Stop before SIGTERM
	killWait time.Duration // wait after SIGTERM before SIGKILL
}

// defaultCleanupTimeouts is the production escalation budget:
// Stop-if-known → wait 5s → SIGTERM → wait 10s → SIGKILL → reap, within 30s.
var defaultCleanupTimeouts = cleanupTimeouts{
	overall:  30 * time.Second,
	stop:     5 * time.Second,
	termWait: 5 * time.Second,
	killWait: 10 * time.Second,
}

// cleanupFailedSpawn runs the cleanup escalation sequence with the production
// timeout budget. This function runs in a goroutine that is intentionally NOT
// tracked in monitorWG (as per the plan: spawn waiter excluded from manager
// shutdown waits).
func (m *Manager) cleanupFailedSpawn(pending *pendingRoot) {
	m.cleanupFailedSpawnWithTimeouts(pending, defaultCleanupTimeouts)
}

// cleanupFailedSpawnWithTimeouts performs the mandated cleanup ordering:
// graceful Stop (if the root ID is known) → wait → SIGTERM → wait → SIGKILL →
// reap and best-effort socket unlink. Timeouts are injectable for tests.
func (m *Manager) cleanupFailedSpawnWithTimeouts(pending *pendingRoot, t cleanupTimeouts) {
	waitUntil := func(ctx context.Context, done <-chan struct{}, duration time.Duration) bool {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-done:
			return true
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		}
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), t.overall)
	defer cancel()

	// Graceful stop first if the root became responsive and reported its ID.
	stopCtx, stopCancel := context.WithTimeout(cleanupCtx, t.stop)
	_ = pending.stopIfResponsive(stopCtx)
	stopCancel()

	// Close client after stop attempt.
	if pending.client != nil {
		_ = pending.client.Close()
		pending.client = nil
	}

	if !waitUntil(cleanupCtx, pending.process.Done(), t.termWait) {
		_ = pending.process.SignalGroup(syscall.SIGTERM)
	}
	if !waitUntil(cleanupCtx, pending.process.Done(), t.killWait) {
		_ = pending.process.SignalGroup(syscall.SIGKILL)
	}
	select {
	case <-pending.process.Done():
	case <-cleanupCtx.Done():
		return
	}
	_ = errors.Join(pending.process.WaitErr(), pending.safeUnlinkOwnedSocket())
}

// spawnTokenBytes is the number of random bytes in a spawn launch token. 16
// bytes = 128 bits of entropy, ample for an unguessable launch token, and its
// hex rendering (spawnTokenHexLen chars) leaves far more of the 107-byte unix
// socket path budget for the directory than the previous 32-byte/64-hex token.
const spawnTokenBytes = 16

// spawnTokenHexLen is the length of a spawn token rendered as lowercase hex.
// New()'s path-length probe uses this so the probe reflects the real filename.
const spawnTokenHexLen = spawnTokenBytes * 2 // hex.EncodedLen(spawnTokenBytes)

func generateToken() (string, error) {
	var buf [spawnTokenBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
