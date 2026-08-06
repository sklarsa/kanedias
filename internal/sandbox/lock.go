package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func validateName(name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("invalid sandbox name %q", name)
	}
	return nil
}

func acquireLifecycleLock(name string) (io.Closer, error) {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("kanedias-sandbox-locks-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create sandbox lock directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, fmt.Errorf("set sandbox lock directory permissions: %w", err)
	}

	file, err := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open sandbox lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another lifecycle operation is active for %q", name)
		}
		return nil, fmt.Errorf("lock sandbox %q: %w", name, err)
	}
	return &lifecycleLock{file: file}, nil
}

type lifecycleLock struct {
	file *os.File
}

func (lock *lifecycleLock) Close() error {
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}
