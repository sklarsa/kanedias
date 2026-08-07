package supervisorapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const unixShutdownTimeout = 5 * time.Second

func ServeUnix(ctx context.Context, path string, handler http.Handler) (err error) {
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("Unix socket path must be absolute")
	}
	if handler == nil {
		return fmt.Errorf("Unix socket handler is required")
	}
	if err := prepareUnixPath(path); err != nil {
		return err
	}

	address, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return fmt.Errorf("resolve Unix socket %q: %w", path, err)
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return fmt.Errorf("listen on Unix socket %q: %w", path, err)
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		_ = safeUnlinkSocket(path, nil)
		return fmt.Errorf("set Unix socket %q mode 0600: %w", path, err)
	}
	bound, err := socketIdentity(path)
	if err != nil {
		_ = safeUnlinkSocket(path, nil)
		return err
	}
	defer func() { err = errors.Join(err, safeUnlinkSocket(path, bound)) }()

	server := &http.Server{Handler: handler}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()

	select {
	case serveErr := <-serveResult:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unixShutdownTimeout)
		defer cancel()
		if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
			_ = server.Close()
			return shutdownErr
		}
		serveErr := <-serveResult
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
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
