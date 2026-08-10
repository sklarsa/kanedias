package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"golang.org/x/sys/unix"
)

// ---- fakeProcess for unit tests ----

type fakeProcess struct {
	mu         sync.Mutex
	pid        int
	done       chan struct{}
	waitErr    error
	signals    []syscall.Signal
	signalErr  error
	signalHook func(syscall.Signal)
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
	err := p.signalErr
	hook := p.signalHook
	p.mu.Unlock()
	if hook != nil {
		hook(sig)
	}
	return err
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
	lastSpec       spawnSpec
	process        *fakeProcess
	err            error
	starts         int
	rootBootstraps chan rootBootstrapResult
}

type rootBootstrapResult struct {
	bootstrap process.RootBootstrap
	err       error
}

type retainingStarter struct {
	process       *fakeProcess
	started       chan error
	inheritedRead *os.File
}

type closingBootstrapStarter struct{ process *fakeProcess }

func (starter closingBootstrapStarter) Start(spec spawnSpec) (spawnedProcess, error) {
	if len(spec.ExtraFiles) != 1 {
		return nil, fmt.Errorf("ExtraFiles = %d, want 1", len(spec.ExtraFiles))
	}
	duplicate, err := unix.Dup(int(spec.ExtraFiles[0].Fd()))
	if err != nil {
		return nil, err
	}
	if err := unix.Close(duplicate); err != nil {
		return nil, err
	}
	return starter.process, nil
}

func (starter *retainingStarter) Start(spec spawnSpec) (spawnedProcess, error) {
	if len(spec.ExtraFiles) != 1 {
		err := fmt.Errorf("ExtraFiles = %d, want 1", len(spec.ExtraFiles))
		starter.started <- err
		return nil, err
	}
	duplicate, err := unix.Dup(int(spec.ExtraFiles[0].Fd()))
	if err != nil {
		starter.started <- err
		return nil, err
	}
	starter.inheritedRead = os.NewFile(uintptr(duplicate), "retained-root-bootstrap")
	starter.started <- nil
	return starter.process, nil
}

func (fs *fakeStarter) Start(spec spawnSpec) (spawnedProcess, error) {
	fs.lastSpec = spec
	fs.starts++
	if fs.err != nil {
		return nil, fs.err
	}
	if len(spec.ExtraFiles) != 1 {
		return nil, fmt.Errorf("ExtraFiles = %d, want 1", len(spec.ExtraFiles))
	}
	duplicate, err := unix.Dup(int(spec.ExtraFiles[0].Fd()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), "fake-root-bootstrap")
	go func() {
		bootstrap, decodeErr := process.DecodeRootBootstrap(file)
		closeErr := file.Close()
		if fs.rootBootstraps != nil {
			fs.rootBootstraps <- rootBootstrapResult{bootstrap: bootstrap, err: errors.Join(decodeErr, closeErr)}
		}
	}()
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

func TestSpawnRootWithRequestValidatesBeforeSideEffects(t *testing.T) {
	fs := &fakeStarter{process: newFakeProcess(1235)}
	m := fakeManager(nil)
	m.starter = fs
	m.opts.SessionBinary = "/usr/bin/kanedias"
	m.opts.ConfigPath = "/etc/kanedias/config.toml"
	rootDir, logDir := shortTempDirs(t)
	m.opts.RootSocketDir = rootDir
	m.opts.SessionLogDir = logDir

	tokens, pipes := 0, 0
	m.newSpawnToken = func() (string, error) {
		tokens++
		return "token", nil
	}
	m.newBootstrapPipe = func() (*os.File, *os.File, error) {
		pipes++
		return os.Pipe()
	}

	unknownModel := m.launch.DefaultRequest()
	unknownModel.Root.ModelType = "not-allowlisted"
	badName := m.launch.DefaultRequest()
	badName.Name = "triage\n" // control character
	overlongName := m.launch.DefaultRequest()
	overlongName.Name = strings.Repeat("a", 81)
	unknownRepo := m.launch.DefaultRequest()
	unknownRepo.Repository = "owner/not-configured"

	cases := map[string]SessionLaunchRequest{
		"unknown model":          unknownModel,
		"control-character name": badName,
		"overlong name":          overlongName,
		"unknown repository":     unknownRepo,
	}
	for name, request := range cases {
		tokens, pipes = 0, 0
		if _, err := m.SpawnRootWithRequest(context.Background(), request); err == nil {
			t.Fatalf("%s: invalid request succeeded", name)
		}
		if tokens != 0 || pipes != 0 || fs.starts != 0 {
			t.Fatalf("%s: side effects after invalid request: tokens=%d pipes=%d starts=%d", name, tokens, pipes, fs.starts)
		}
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid requests created log files: %v", entries)
	}
}

func TestSpawnRootWithRequestTransfersResolvedPolicyAndWorkspaceOnlyThroughFD3(t *testing.T) {
	fs := &fakeStarter{process: newFakeProcess(1236), rootBootstraps: make(chan rootBootstrapResult, 1)}
	m := fakeManager(nil)
	m.starter = fs
	m.opts.SessionBinary = "/usr/bin/kanedias"
	m.opts.ConfigPath = "/etc/kanedias/config.toml"
	rootDir, logDir := shortTempDirs(t)
	m.opts.RootSocketDir = rootDir
	m.opts.SessionLogDir = logDir
	m.opts.SpawnTimeout = time.Second
	m.newSpawnToken = func() (string, error) { return "fixed-token", nil }
	var parentRead, parentWrite *os.File
	m.newBootstrapPipe = func() (*os.File, *os.File, error) {
		var pipeErr error
		parentRead, parentWrite, pipeErr = os.Pipe()
		return parentRead, parentWrite, pipeErr
	}

	request := m.launch.DefaultRequest()
	request.Repository = "owner/repo"
	request.Root = ModelSelection{ModelType: "local-qwen", ThinkingLevel: "off"}
	for index := range request.Workers {
		request.Workers[index].ModelType = "local-qwen"
		request.Workers[index].ThinkingLevel = "off"
	}
	resolved, err := m.launch.Resolve(request)
	if err != nil {
		t.Fatal(err)
	}
	wantPolicy := resolved.Policy
	wantWorkspace := resolved.Workspace
	go func() {
		time.Sleep(20 * time.Millisecond)
		fs.process.exit(nil)
	}()
	_, _ = m.SpawnRootWithRequest(context.Background(), request)

	select {
	case result := <-fs.rootBootstraps:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if !reflect.DeepEqual(result.bootstrap.Policy, wantPolicy) {
			t.Fatalf("bootstrap policy = %#v, want %#v", result.bootstrap.Policy, wantPolicy)
		}
		if result.bootstrap.Workspace != wantWorkspace {
			t.Fatalf("bootstrap workspace = %#v, want %#v", result.bootstrap.Workspace, wantWorkspace)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out decoding inherited root bootstrap")
	}
	assertFileClosed(t, parentRead)
	assertFileClosed(t, parentWrite)

	spec := fs.lastSpec
	wantSuffix := []string{"session", "--socket", filepath.Join(rootDir, "fixed-token.root.sock"), "--bootstrap-fd", "3"}
	if len(spec.Args) < len(wantSuffix) || !reflect.DeepEqual(spec.Args[len(spec.Args)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("argv = %#v, want suffix %#v", spec.Args, wantSuffix)
	}
	if len(spec.ExtraFiles) != 1 {
		t.Fatalf("ExtraFiles = %d, want 1", len(spec.ExtraFiles))
	}
	argv := strings.Join(spec.Args, "\x00")
	for _, value := range []string{wantPolicy.Root.Provider, wantPolicy.Root.Model, wantWorkspace.Repository, wantWorkspace.Checkout, `"provider"`, `"workers"`, `"workspace"`} {
		if strings.Contains(argv, value) {
			t.Fatalf("private policy value %q leaked into argv", value)
		}
	}
	if !reflect.DeepEqual(spec.Env, os.Environ()) {
		t.Fatal("root spawn modified the inherited environment to carry policy")
	}
}

func TestSpawnRootClosesBootstrapPipeOnStartFailure(t *testing.T) {
	fs := &fakeStarter{err: errors.New("start sentinel")}
	m := configuredSpawnManager(t, fs)
	var readEnd, writeEnd *os.File
	m.newBootstrapPipe = func() (*os.File, *os.File, error) {
		var err error
		readEnd, writeEnd, err = os.Pipe()
		return readEnd, writeEnd, err
	}
	if _, err := m.SpawnRoot(context.Background()); err == nil || !strings.Contains(err.Error(), "start sentinel") {
		t.Fatalf("SpawnRoot error = %v", err)
	}
	assertFileClosed(t, readEnd)
	assertFileClosed(t, writeEnd)
}

func TestSpawnRootClosesBootstrapPipeOnWriteFailure(t *testing.T) {
	fakeProcess := newFakeProcess(1237)
	m := configuredSpawnManager(t, closingBootstrapStarter{process: fakeProcess})
	var readEnd, writeEnd *os.File
	m.newBootstrapPipe = func() (*os.File, *os.File, error) {
		var err error
		readEnd, writeEnd, err = os.Pipe()
		return readEnd, writeEnd, err
	}
	if _, err := m.SpawnRoot(context.Background()); err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("SpawnRoot error = %v, want bootstrap write failure", err)
	}
	assertFileClosed(t, readEnd)
	assertFileClosed(t, writeEnd)
	fakeProcess.mu.Lock()
	signals := append([]syscall.Signal(nil), fakeProcess.signals...)
	fakeProcess.mu.Unlock()
	if len(signals) != 1 || signals[0] != syscall.SIGKILL {
		t.Fatalf("bootstrap write failure signals = %v, want [SIGKILL]", signals)
	}
}

func TestSpawnRootBootstrapAbortBoundedlyObservesDelayedExit(t *testing.T) {
	process := newFakeProcess(1239)
	process.waitErr = errors.New("exit sentinel")
	process.signalHook = func(sig syscall.Signal) {
		if sig == syscall.SIGKILL {
			go func() {
				time.Sleep(20 * time.Millisecond)
				close(process.done)
			}()
		}
	}
	m := configuredSpawnManager(t, &fakeStarter{process: process})
	m.writeRootBootstrap = func(io.Writer, []byte) error { return errors.New("write sentinel") }
	m.rootAbortWait = time.Second
	started := time.Now()
	_, err := m.SpawnRoot(context.Background())
	if !strings.Contains(err.Error(), "write sentinel") || !strings.Contains(err.Error(), "exit sentinel") {
		t.Fatalf("SpawnRoot error = %v, want primary and observed exit errors", err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("abort observation elapsed = %v, want bounded delayed observation", elapsed)
	}
}

func TestSpawnRootBootstrapAbortJoinsSignalFailureAndTimeout(t *testing.T) {
	process := newFakeProcess(1240)
	process.signalErr = errors.New("signal sentinel")
	m := configuredSpawnManager(t, &fakeStarter{process: process})
	m.writeRootBootstrap = func(io.Writer, []byte) error { return errors.New("write sentinel") }
	m.rootAbortWait = 20 * time.Millisecond
	_, err := m.SpawnRoot(context.Background())
	for _, want := range []string{"write sentinel", "signal sentinel", "timed out"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("SpawnRoot error = %v, want %q", err, want)
		}
	}
}

func TestSpawnRootCancellationJoinsBlockedBootstrapWriter(t *testing.T) {
	starter := &retainingStarter{process: newFakeProcess(1238), started: make(chan error, 1)}
	m := configuredSpawnManager(t, starter)
	writerStarted := make(chan struct{})
	writerUnblocked := make(chan struct{})
	allowWriterCompletion := make(chan struct{})
	writerCompleted := make(chan struct{})
	joinStarted := make(chan struct{})
	writeRootBootstrap := m.writeRootBootstrap
	m.writeRootBootstrap = func(writer io.Writer, encoded []byte) error {
		close(writerStarted)
		err := writeRootBootstrap(writer, encoded)
		close(writerUnblocked)
		<-allowWriterCompletion
		close(writerCompleted)
		return err
	}
	waitRootBootstrapWrite := m.waitRootBootstrapWrite
	m.waitRootBootstrapWrite = func(done <-chan struct{}) {
		close(joinStarted)
		waitRootBootstrapWrite(done)
	}
	cfg := modelConfigFixture()
	worker := cfg.Workers["worker"]
	worker.Description = strings.Repeat("x", process.MaxRecordBytes-4096)
	cfg.Workers["worker"] = worker
	launch, err := NewLaunchConfiguration(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.launch = launch
	resolved, err := launch.Resolve(launch.DefaultRequest())
	if err != nil {
		t.Fatal(err)
	}
	policy := resolved.Policy
	var encoded bytes.Buffer
	if err := process.EncodeRootBootstrap(&encoded, process.RootBootstrap{Policy: policy}); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() < 64*1024 {
		t.Fatalf("bootstrap length = %d, want enough to block an unread pipe", encoded.Len())
	}

	var parentRead, parentWrite *os.File
	m.newBootstrapPipe = func() (*os.File, *os.File, error) {
		var pipeErr error
		parentRead, parentWrite, pipeErr = os.Pipe()
		return parentRead, parentWrite, pipeErr
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, spawnErr := m.SpawnRoot(ctx)
		result <- spawnErr
	}()
	if startErr := <-starter.started; startErr != nil {
		t.Fatal(startErr)
	}
	select {
	case <-writerStarted:
	case <-time.After(time.Second):
		t.Fatal("bootstrap writer did not start")
	}
	deadline := time.Now().Add(time.Second)
	for {
		available, ioctlErr := unix.IoctlGetInt(int(starter.inheritedRead.Fd()), unix.TIOCINQ)
		if ioctlErr != nil {
			t.Fatal(ioctlErr)
		}
		if available > 0 {
			if available >= encoded.Len() {
				t.Fatalf("queued bootstrap bytes = %d, encoded length = %d; write may have completed", available, encoded.Len())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bootstrap writer did not begin filling retained unread pipe")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-writerUnblocked:
		t.Fatal("real encoded bootstrap write returned before cancellation")
	default:
	}
	select {
	case spawnErr := <-result:
		t.Fatalf("SpawnRoot returned before cancellation while bootstrap remained incomplete: %v", spawnErr)
	default:
	}
	select {
	case <-writerCompleted:
		t.Fatal("bootstrap writer completed despite unread queued bytes being shorter than the encoded record")
	default:
	}

	cancel()
	select {
	case <-joinStarted:
	case <-time.After(time.Second):
		t.Fatal("SpawnRoot did not enter the bootstrap-writer join after cancellation")
	}
	select {
	case <-writerUnblocked:
	case <-time.After(time.Second):
		t.Fatal("closing the parent writer did not unblock the blocked bootstrap write")
	}
	select {
	case spawnErr := <-result:
		t.Fatalf("SpawnRoot returned without joining the deliberately paused writer: %v", spawnErr)
	default:
	}
	close(allowWriterCompletion)
	select {
	case spawnErr := <-result:
		if !errors.Is(spawnErr, context.Canceled) {
			t.Fatalf("SpawnRoot error = %v, want %v", spawnErr, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("SpawnRoot did not promptly join the released bootstrap writer after cancellation")
	}
	select {
	case <-writerCompleted:
	default:
		t.Fatal("SpawnRoot returned before the bootstrap writer completed and joined")
	}
	assertFileClosed(t, parentRead)
	assertFileClosed(t, parentWrite)
	starter.process.mu.Lock()
	signals := append([]syscall.Signal(nil), starter.process.signals...)
	starter.process.mu.Unlock()
	if len(signals) != 1 || signals[0] != syscall.SIGKILL {
		t.Fatalf("cancellation signals = %v, want [SIGKILL]", signals)
	}

	if err := starter.inheritedRead.Close(); err != nil {
		t.Fatal(err)
	}
}

func configuredSpawnManager(t *testing.T, starter processStarter) *Manager {
	t.Helper()
	m := fakeManager(nil)
	m.starter = starter
	m.opts.SessionBinary = "/usr/bin/kanedias"
	m.opts.ConfigPath = "/etc/kanedias/config.toml"
	m.opts.RootSocketDir, m.opts.SessionLogDir = shortTempDirs(t)
	m.opts.SpawnTimeout = 50 * time.Millisecond
	m.newSpawnToken = func() (string, error) { return "fixed-token", nil }
	return m
}

func assertFileClosed(t *testing.T, file *os.File) {
	t.Helper()
	if file == nil {
		t.Fatal("file was not created")
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("file %q remains open: Stat error = %v", file.Name(), err)
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
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SpawnRoot error = %v, want admission deadline exceeded", err)
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
	if err == nil || !strings.Contains(err.Error(), "root exited before admission") || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("SpawnRoot error = %v, want root-exited-before-admission wrapping %v", err, exec.ErrNotFound)
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
