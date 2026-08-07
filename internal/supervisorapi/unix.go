package supervisorapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const unixShutdownTimeout = 5 * time.Second

type UnixServer struct {
	path     string
	bound    *unixSocketIdentity
	listener *net.UnixListener
	server   *http.Server
	done     chan struct{}

	closeOnce sync.Once
	mu        sync.Mutex
	err       error
}

// StartUnix binds the mode-0600 socket synchronously before serving. Callers can
// therefore provision a guest proxy only after the host endpoint is live.
func StartUnix(path string, handler http.Handler) (*UnixServer, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("Unix socket path must be absolute")
	}
	if handler == nil {
		return nil, fmt.Errorf("Unix socket handler is required")
	}
	if err := prepareUnixPath(path); err != nil {
		return nil, err
	}
	address, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, fmt.Errorf("resolve Unix socket %q: %w", path, err)
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, fmt.Errorf("listen on Unix socket %q: %w", path, err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = safeUnlinkSocket(path, nil)
		return nil, fmt.Errorf("set Unix socket %q mode 0600: %w", path, err)
	}
	bound, err := socketIdentity(path)
	if err != nil {
		_ = listener.Close()
		_ = safeUnlinkSocket(path, nil)
		return nil, err
	}
	result := &UnixServer{path: path, bound: bound, listener: listener, server: &http.Server{Handler: handler}, done: make(chan struct{})}
	go func() {
		serveErr := result.server.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		closeErr := listener.Close()
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
		result.mu.Lock()
		result.err = errors.Join(serveErr, closeErr, safeUnlinkSocket(path, bound))
		result.mu.Unlock()
		close(result.done)
	}()
	return result, nil
}

func (server *UnixServer) Done() <-chan struct{} { return server.done }

func (server *UnixServer) Err() error {
	<-server.done
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.err
}

// Close cancels serving and waits for all listener cleanup, including unlink.
func (server *UnixServer) Close(ctx context.Context) error {
	var shutdownErr error
	server.closeOnce.Do(func() {
		shutdownErr = server.server.Shutdown(ctx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, server.server.Close())
		}
	})
	select {
	case <-server.done:
		return errors.Join(shutdownErr, server.Err())
	case <-ctx.Done():
		_ = server.server.Close()
		<-server.done
		return errors.Join(shutdownErr, ctx.Err(), server.Err())
	}
}

func ServeUnix(ctx context.Context, path string, handler http.Handler) error {
	server, err := StartUnix(path, handler)
	if err != nil {
		return err
	}
	select {
	case <-server.Done():
		return server.Err()
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unixShutdownTimeout)
		defer cancel()
		return server.Close(shutdownCtx)
	}
}

func prepareUnixPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Unix socket %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse symlink at Unix socket path %q", path)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refuse non-socket at Unix socket path %q", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("refuse Unix socket %q not owned by effective user", path)
	}
	connection, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("Unix socket %q is already accepting connections", path)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("refuse to replace unverifiably stale Unix socket %q: %w", path, dialErr)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale Unix socket %q: %w", path, err)
	}
	return nil
}

type unixSocketIdentity struct {
	device uint64
	inode  uint64
}

func socketIdentity(path string) (*unixSocketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect bound Unix socket %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("bound Unix path %q is not a socket", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("inspect bound Unix socket %q identity", path)
	}
	return &unixSocketIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func safeUnlinkSocket(path string, expected *unixSocketIdentity) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Unix socket %q for unlink: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refuse unlink of replaced Unix socket path %q", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect Unix socket %q identity for unlink", path)
	}
	if expected != nil && (uint64(stat.Dev) != expected.device || stat.Ino != expected.inode) {
		return fmt.Errorf("refuse unlink of replaced Unix socket %q", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unlink Unix socket %q: %w", path, err)
	}
	return nil
}
