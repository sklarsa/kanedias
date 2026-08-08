package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

// spawnSpec describes a process to be started.
type spawnSpec struct {
	Path        string
	Args        []string
	Env         []string
	Stdin       *os.File
	Output      *os.File
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
	cmd.SysProcAttr = spec.SysProcAttr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	started := &startedProcess{cmd: cmd, done: make(chan struct{})}
	go started.waitExactlyOnce()
	return started, nil
}

// pendingRoot tracks a root that is awaiting admission.
type pendingRoot struct {
	socketPath string
	identity   socketIdentity
	logPath    string
	process    spawnedProcess
	client     rootClient
	rootID     string // set after first successful snapshot
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

// safeUnlinkOwnedSocket removes the socket file only if it still has the exact
// same device+inode as when the manager created it.
func (pending *pendingRoot) safeUnlinkOwnedSocket() error {
	if !sameIdentity(pending.socketPath, pending.identity) {
		return nil
	}
	return os.Remove(pending.socketPath)
}

// SpawnRoot launches a detached root supervisor and admits it into the fleet.
func (m *Manager) SpawnRoot(ctx context.Context) (string, error) {
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

	token, err := generateToken()
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

	spec := spawnSpec{
		Path: m.opts.SessionBinary,
		Args: []string{
			m.opts.SessionBinary,
			"--config", m.opts.ConfigPath,
			"session",
			"--socket", socketPath,
		},
		Env:    os.Environ(),
		Stdin:  devNull,
		Output: logFile,
		SysProcAttr: &syscall.SysProcAttr{
			Setsid: true,
		},
	}

	process, err := m.starter.Start(spec)
	// Close parent file descriptors; child has inherited them.
	_ = logFile.Close()
	_ = devNull.Close()
	if err != nil {
		_ = os.Remove(logPath)
		return "", fmt.Errorf("manager: start root process: %w", err)
	}

	pending := &pendingRoot{
		socketPath: socketPath,
		logPath:    logPath,
		process:    process,
	}

	rootID, err := m.admitRoot(ctx, pending)
	if err != nil {
		go m.cleanupFailedSpawn(pending)
		return "", err
	}
	return rootID, nil
}

// admitRoot polls the pending root until it becomes admissible or gives up.
func (m *Manager) admitRoot(ctx context.Context, pending *pendingRoot) (string, error) {
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
		case <-pending.process.Done():
			return "", fmt.Errorf("root exited before admission: %w", pending.process.WaitErr())
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

			lc := supervisor.LifecycleState(snapshot.Lifecycle)
			if lc == supervisor.LifecycleProvisioning || lc == supervisor.LifecycleStarting {
				continue
			}
			if !admissible(snapshot) {
				continue
			}
			// Atomic commit.
			return m.commitSpawn(pending, snapshot)
		}
	}
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
	if err := m.commitTree(handle, normalized, candidate); err != nil {
		return "", fmt.Errorf("spawn admission route conflict: %w", err)
	}
	// Transfer client ownership — pending.client is now owned by the handle.
	pending.client = nil

	// Start monitoring the newly admitted root.
	m.mu.Lock()
	h := m.roots[pending.socketPath]
	m.mu.Unlock()
	if h != nil {
		m.monitorRoot(h)
	}
	m.bumpFleetRevision()
	return snapshot.SessionID, nil
}

// cleanupFailedSpawn runs the 30-second cleanup escalation sequence. This
// function runs in a goroutine that is intentionally NOT tracked in monitorWG
// (as per the plan: spawn waiter excluded from manager shutdown waits).
func (m *Manager) cleanupFailedSpawn(pending *pendingRoot) {
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

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stopCtx, stopCancel := context.WithTimeout(cleanupCtx, 5*time.Second)
	_ = pending.stopIfResponsive(stopCtx)
	stopCancel()

	// Close client after stop attempt.
	if pending.client != nil {
		_ = pending.client.Close()
		pending.client = nil
	}

	if !waitUntil(cleanupCtx, pending.process.Done(), 5*time.Second) {
		_ = pending.process.SignalGroup(syscall.SIGTERM)
	}
	if !waitUntil(cleanupCtx, pending.process.Done(), 10*time.Second) {
		_ = pending.process.SignalGroup(syscall.SIGKILL)
	}
	select {
	case <-pending.process.Done():
	case <-cleanupCtx.Done():
		return
	}
	_ = errors.Join(pending.process.WaitErr(), pending.safeUnlinkOwnedSocket())
}

func generateToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
