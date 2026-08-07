package incusworkspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquireSeedLock(pool, seed string, exclusive bool) (io.Closer, error) {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("kanedias-incus-seed-locks-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create nested Incus seed lock directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("set nested Incus seed lock directory permissions: %w", err)
	}

	digest := sha256.Sum256([]byte(pool + "\x00" + seed))
	path := filepath.Join(dir, hex.EncodeToString(digest[:])+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open nested Incus seed lock: %w", err)
	}

	operation := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		operation = unix.LOCK_EX | unix.LOCK_NB
	}
	if err := unix.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another operation is active for nested Incus seed %q", seed)
		}
		return nil, fmt.Errorf("lock nested Incus seed %q: %w", seed, err)
	}
	return &seedLock{file: file}, nil
}

type seedLock struct {
	file *os.File
}

func (lock *seedLock) Close() error {
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}
