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

	"golang.org/x/sys/unix"
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
		return nil, fmt.Errorf("socket path must be absolute for the Unix listener")
	}
	if handler == nil {
		return nil, fmt.Errorf("handler is required for the Unix listener")
	}
	if len(path) >= len(syscall.RawSockaddrUnix{}.Path) {
		return nil, fmt.Errorf("socket path %q exceeds the platform address bound for the Unix listener", path)
	}
	if err := prepareUnixPath(path); err != nil {
		return nil, err
	}
	// Bind below a private 0700 directory, harden the socket, then atomically
	// publish it without replacement. The public API path is never connectable
	// with broader permissions.
	temporaryDir, err := privateUnixBindDirectory(path)
	if err != nil {
		return nil, err
	}
	temporaryPath := filepath.Join(temporaryDir, "s")
	cleanupTemporary := func() { _ = os.RemoveAll(temporaryDir) }
	address, err := net.ResolveUnixAddr("unix", temporaryPath)
	if err != nil {
		cleanupTemporary()
		return nil, fmt.Errorf("resolve temporary Unix socket %q: %w", temporaryPath, err)
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		cleanupTemporary()
		return nil, fmt.Errorf("listen on temporary Unix socket %q: %w", temporaryPath, err)
	}
	listener.SetUnlinkOnClose(false)
	temporaryIdentity, identityErr := socketIdentity(temporaryPath)
	if identityErr != nil {
		_ = listener.Close()
		cleanupTemporary()
		return nil, identityErr
	}
	failBound := func() {
		_ = listener.Close()
		_ = safeUnlinkSocket(temporaryPath, temporaryIdentity)
		cleanupTemporary()
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		failBound()
		return nil, fmt.Errorf("set Unix socket %q mode 0600: %w", path, err)
	}
	if err := unix.Renameat2(unix.AT_FDCWD, temporaryPath, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE); err != nil {
		failBound()
		return nil, fmt.Errorf("publish Unix socket %q atomically: %w", path, err)
	}
	cleanupTemporary()
	bound, err := socketIdentity(path)
	if err != nil {
		_ = listener.Close()
		_ = safeUnlinkSocket(path, temporaryIdentity)
		return nil, err
	}
	result := &UnixServer{path: path, bound: bound, listener: listener, server: &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}, done: make(chan struct{})}
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

func privateUnixBindDirectory(finalPath string) (string, error) {
	parent := filepath.Dir(finalPath)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect Unix socket parent %q: %w", parent, err)
	}
	parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("inspect Unix socket parent %q device", parent)
	}
	var lastErr error
	for candidate := parent; ; candidate = filepath.Dir(candidate) {
		info, err := os.Stat(candidate)
		if err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Dev == parentStat.Dev {
				directory, mkdirErr := os.MkdirTemp(candidate, ".k-")
				if mkdirErr == nil {
					if len(filepath.Join(directory, "s")) < len(syscall.RawSockaddrUnix{}.Path) {
						return directory, nil
					}
					_ = os.RemoveAll(directory)
				} else {
					lastErr = mkdirErr
				}
			}
		}
		if candidate == filepath.Dir(candidate) {
			break
		}
	}
	return "", fmt.Errorf("create same-filesystem private Unix bind directory for %q: %w", finalPath, lastErr)
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
		return fmt.Errorf("socket %q is already accepting connections on the Unix listener", path)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("refuse to replace unverifiably stale Unix socket %q: %w", path, dialErr)
	}
	expected, err := identityFromInfo(info)
	if err != nil {
		return fmt.Errorf("inspect stale Unix socket %q identity: %w", path, err)
	}
	if err := safeUnlinkSocket(path, expected); err != nil {
		return fmt.Errorf("remove stale Unix socket %q: %w", path, err)
	}
	return nil
}

type unixSocketIdentity struct {
	device uint64
	inode  uint64
}

func identityFromInfo(info os.FileInfo) (*unixSocketIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("socket identity is unavailable")
	}
	return &unixSocketIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func socketIdentity(path string) (*unixSocketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect bound Unix socket %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("bound Unix path %q is not a socket", path)
	}
	identity, err := identityFromInfo(info)
	if err != nil {
		return nil, fmt.Errorf("inspect bound Unix socket %q identity: %w", path, err)
	}
	return identity, nil
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
