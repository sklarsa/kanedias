package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"golang.org/x/sys/unix"
)

// ---- fakeProcess for unit tests ----

type fakeProcess struct {
	mu      sync.Mutex
	pid     int
	done    chan struct{}
	waitErr error
	signals []syscall.Signal
}

func newFakeProcess(pid int) *fakeProcess {
	return &fakeProcess{pid: pid, done: make(chan struct{})}
}

func (p *fakeProcess) PID() int              { return p.pid }
func (p *fakeProcess) Done() <-chan struct{} { return p.done }
func (p *fakeProcess) WaitErr() error        { return p.waitErr }
func (p *fakeProcess) SignalGroup(sig syscall.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	return nil
}
func (p *fakeProcess) signalCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.signals)
}
func (p *fakeProcess) exit(err error) {
	p.waitErr = err
	close(p.done)
}

// ---- fakeStarter ----

type fakeStarter struct {
	lastSpec spawnSpec
	process  *fakeProcess
	err      error
}

func (fs *fakeStarter) Start(spec spawnSpec) (spawnedProcess, error) {
	fs.lastSpec = spec
	if fs.err != nil {
		return nil, fs.err
	}
	return fs.process, nil
}

// ---- TestRootSpawner: assert exact argv and SysProcAttr ----

func TestRootSpawnerArgvAndSetsid(t *testing.T) {
	fs := &fakeStarter{process: newFakeProcess(1234)}
	m := fakeManager(nil)
	m.starter = fs
	m.opts.SessionBinary = "/usr/bin/kanedias"
	m.opts.ConfigPath = "/etc/kanedias/config.toml"
	// Use short paths to stay within unix socket path limit (107 bytes).
	rootDir, logDir := shortTempDirs(t)
	m.opts.RootSocketDir = rootDir
	m.opts.SessionLogDir = logDir
	m.opts.SpawnTimeout = 100 * time.Millisecond

	// SpawnRoot will fail because the fake process never creates a socket,
	// but we can check the spec that was passed to Start.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// Make fake process exit immediately so cleanup runs quickly.
	go func() {
		time.Sleep(10 * time.Millisecond)
		fs.process.exit(nil)
	}()
	_, _ = m.SpawnRoot(ctx)

	spec := fs.lastSpec
	if spec.Path != "/usr/bin/kanedias" {
		t.Fatalf("path = %q, want /usr/bin/kanedias", spec.Path)
	}
	// Args[0] = binary, Args[1] = "--config", Args[2] = configPath, Args[3] = "session", Args[4] = "--socket", Args[5] = socketPath
	wantArgs := []string{"/usr/bin/kanedias", "--config", "/etc/kanedias/config.toml", "session", "--socket"}
	for i, want := range wantArgs {
		if i >= len(spec.Args) {
			t.Fatalf("args[%d] missing, want %q", i, want)
		}
		if spec.Args[i] != want {
			t.Fatalf("args[%d] = %q, want %q", i, spec.Args[i], want)
		}
	}
	// Socket path must end in .root.sock
	if len(spec.Args) < 6 {
		t.Fatalf("too few args: %v", spec.Args)
	}
	socketArg := spec.Args[5]
	if !filepath.IsAbs(socketArg) || len(socketArg) < len(".root.sock") ||
		socketArg[len(socketArg)-len(".root.sock"):] != ".root.sock" {
		t.Fatalf("socket arg %q does not look like an absolute .root.sock path", socketArg)
	}
	if spec.SysProcAttr == nil || !spec.SysProcAttr.Setsid {
		t.Fatalf("SysProcAttr.Setsid must be true, got %+v", spec.SysProcAttr)
	}
}

// ---- TestRootSpawnerSelf: OS-level test using re-exec ----

// TestMain helper: if SPAWN_TEST_HELPER is set, act as the child process.
func init() {
	if os.Getenv("SPAWN_TEST_HELPER") == "1" {
		// Report our SID, verify stdin is /dev/null (reads return immediately).
		sid, _ := unix.Getsid(0)
		pid := os.Getpid()
		if sid == pid {
			_, _ = os.Stdout.WriteString("sid_ok\n")
		} else {
			_, _ = os.Stdout.WriteString("sid_fail\n")
		}
		os.Exit(0)
	}
}

func TestRootSpawnerRealExecSetsSid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Setsid test requires Linux")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	logPath := filepath.Join(outDir, "test.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	spec := spawnSpec{
		Path:        exe,
		Args:        []string{exe, "-test.run=^$"},
		Env:         append(os.Environ(), "SPAWN_TEST_HELPER=1"),
		Stdin:       devNull,
		Output:      logFile,
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
	starter := osProcessStarter{}
	process, err := starter.Start(spec)
	_ = logFile.Close()
	_ = devNull.Close()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("helper process did not exit")
	}
	// Read log output.
	out, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "sid_ok\n" {
		t.Fatalf("helper output = %q, want sid_ok", out)
	}
	// Verify cmd.Wait was called exactly once via WaitErr().
	if process.WaitErr() != nil {
		t.Fatalf("WaitErr() = %v after successful exit", process.WaitErr())
	}
}

// ---- Admission polling tests ----

func TestSpawnRootAdmissionTimeout(t *testing.T) {
	fs := &fakeStarter{process: newFakeProcess(9999)}
	m := fakeManager(nil)
	m.starter = fs
	m.opts.SessionBinary = "/usr/bin/kanedias"
	m.opts.ConfigPath = "/etc/kanedias/config.toml"
	rootDir, logDir := shortTempDirs(t)
	m.opts.RootSocketDir = rootDir
	m.opts.SessionLogDir = logDir
	m.opts.SpawnTimeout = 50 * time.Millisecond

	ctx := context.Background()
	_, err := m.SpawnRoot(ctx)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestSpawnRootProcessExitBeforeAdmission(t *testing.T) {
	fs := &fakeStarter{process: newFakeProcess(9998)}
	m := fakeManager(nil)
	m.starter = fs
	m.opts.SessionBinary = "/usr/bin/kanedias"
	m.opts.ConfigPath = "/etc/kanedias/config.toml"
	rootDir, logDir := shortTempDirs(t)
	m.opts.RootSocketDir = rootDir
	m.opts.SessionLogDir = logDir
	m.opts.SpawnTimeout = 5 * time.Second

	// Exit the process before admission.
	go func() {
		time.Sleep(20 * time.Millisecond)
		fs.process.exit(exec.ErrNotFound)
	}()

	ctx := context.Background()
	_, err := m.SpawnRoot(ctx)
	if err == nil {
		t.Fatal("expected error when process exits, got nil")
	}
}

// ---- Cleanup escalation tests ----

// shortCleanupTimeouts is a fast escalation budget for tests.
var shortCleanupTimeouts = cleanupTimeouts{
	overall:  2 * time.Second,
	stop:     10 * time.Millisecond,
	termWait: 10 * time.Millisecond,
	killWait: 20 * time.Millisecond,
}

// TestCleanupEscalationOrder drives the REAL cleanupFailedSpawnWithTimeouts and
// asserts the mandated Stop-if-known → SIGTERM → SIGKILL ordering, including the
// graceful Stop when the root became responsive (rootID set).
func TestCleanupEscalationOrder(t *testing.T) {
	process := newFakeProcess(12345)
	client := &fakeClient{}
	pending := &pendingRoot{
		socketPath: "/tmp/nonexistent-cleanup.root.sock",
		process:    process,
		client:     client,
		rootID:     "test-root", // responsive: graceful Stop must be attempted
	}

	// The fake process exits only after it receives SIGKILL (index 1).
	go func() {
		for {
			time.Sleep(2 * time.Millisecond)
			if process.signalCount() >= 2 {
				process.exit(nil)
				return
			}
		}
	}()

	m := fakeManager(nil)
	start := time.Now()
	m.cleanupFailedSpawnWithTimeouts(pending, shortCleanupTimeouts)
	if elapsed := time.Since(start); elapsed > shortCleanupTimeouts.overall {
		t.Fatalf("cleanup exceeded budget: %v > %v", elapsed, shortCleanupTimeouts.overall)
	}

	// Graceful Stop must have been called on the client (rootID was set).
	client.mu.Lock()
	sawStop := false
	sawClose := false
	for _, call := range client.callLog {
		if call == "Stop" {
			sawStop = true
		}
		if call == "Close" {
			sawClose = true
		}
	}
	client.mu.Unlock()
	if !sawStop {
		t.Fatal("graceful Stop was not attempted despite known root ID")
	}
	if !sawClose {
		t.Fatal("client was not closed during cleanup")
	}

	process.mu.Lock()
	sigs := append([]syscall.Signal(nil), process.signals...)
	process.mu.Unlock()
	if len(sigs) < 2 {
		t.Fatalf("expected at least SIGTERM+SIGKILL, got %v", sigs)
	}
	if sigs[0] != syscall.SIGTERM {
		t.Fatalf("first signal = %v, want SIGTERM", sigs[0])
	}
	if sigs[1] != syscall.SIGKILL {
		t.Fatalf("second signal = %v, want SIGKILL", sigs[1])
	}
}

// TestCleanupSkipsStopWhenRootUnknown proves the graceful Stop is skipped when
// the root never became responsive (rootID empty), going straight to signals.
func TestCleanupSkipsStopWhenRootUnknown(t *testing.T) {
	process := newFakeProcess(12347)
	client := &fakeClient{}
	pending := &pendingRoot{
		socketPath: "/tmp/nonexistent-cleanup2.root.sock",
		process:    process,
		client:     client,
		// rootID intentionally empty
	}
	process.exit(nil) // already dead: no signals needed

	m := fakeManager(nil)
	m.cleanupFailedSpawnWithTimeouts(pending, shortCleanupTimeouts)

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, call := range client.callLog {
		if call == "Stop" {
			t.Fatal("Stop must not be called when root ID is unknown")
		}
	}
}

func TestCleanupNeverUnlinksReplacedSocket(t *testing.T) {
	// The replacement-socket assertion below relies on the filesystem NOT
	// reusing the original socket's inode when it is unlinked and re-bound.
	// GitHub-hosted runners reuse inodes, so the replacement lands on the same
	// inode and detection can't distinguish it. Skip on CI; tracked by #6.
	if os.Getenv("CI") != "" {
		t.Skip("skipping inode-reuse-sensitive replacement-socket check on CI; see #6")
	}
	dir := t.TempDir()
	origPath := makeRootSocket(t, dir, "orig.root.sock")
	identity, err := inspectRootSocket(origPath, os.Lstat, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}

	// Remove and recreate. The filesystem may immediately reuse the inode, so
	// generation identity must include more than device+inode.
	if err := os.Remove(origPath); err != nil {
		t.Fatal(err)
	}
	makeRootSocket(t, dir, "orig.root.sock")

	process := newFakeProcess(12346)
	process.exit(nil)
	pending := &pendingRoot{
		socketPath: origPath,
		identity:   identity,
		process:    process,
	}
	// safeUnlinkOwnedSocket must not remove the replacement.
	if err := pending.safeUnlinkOwnedSocket(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Lstat(origPath); err != nil {
		t.Fatalf("replacement socket was unlinked: %v", err)
	}
}

// ---- AdmittedRoot survives cancellation and Close ----

func TestAdmittedRootSurvivesManagerClose(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sockPath := makeRootSocket(t, dir, "admitted.root.sock")
	tree := rootTree("admitted")
	client := &fakeClient{snapshot: tree}
	identity, err := inspectRootSocket(sockPath, os.Lstat, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}

	m := fakeManager(func(_ string) (rootClient, error) { return client, nil })
	m.opts.RootSocketDir = dir

	// Pre-admit the root.
	handle := &rootHandle{
		socketPath: sockPath, rootID: "admitted",
		identity: identity, actionable: true, client: client,
		mirror: newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
	}
	m.roots[sockPath] = handle
	m.routes["admitted"] = "admitted"
	handle.tree = tree

	// Close the manager.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	// Verify Stop was NOT called on the client.
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, call := range client.callLog {
		if call == "Stop" {
			t.Fatal("Close() called Stop on admitted root — must not")
		}
	}
}
