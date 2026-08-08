package manager

import (
	"fmt"
	"os"
	"syscall"
)

// validatePrivateDir checks that path is an EUID-owned, non-symlink, mode-0700
// directory. It uses os.Lstat so symlinks are never followed.
func validatePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		info.Mode().Perm() != 0o700 || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("private directory %q must be an EUID-owned non-symlink mode-0700 directory", path)
	}
	return nil
}

// socketIdentity captures a socket generation. Device+inode alone is not
// sufficient because filesystems may immediately reuse an inode after unlink;
// the generation's metadata timestamp distinguishes that replacement.
type socketIdentity struct {
	dev       uint64
	ino       uint64
	mtimeNano int64
}

// inspectRootSocket validates that path names a non-symlink Unix-domain socket
// owned by the current EUID with mode 0o600. lstatFn is injected so tests can
// simulate foreign UIDs without requiring chown.
func inspectRootSocket(path string, lstatFn func(string) (os.FileInfo, error), euid int) (socketIdentity, error) {
	info, err := lstatFn(path)
	if err != nil {
		return socketIdentity{}, fmt.Errorf("inspect root socket %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return socketIdentity{}, fmt.Errorf("root socket %q is a symlink", path)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return socketIdentity{}, fmt.Errorf("root socket %q is not a Unix socket", path)
	}
	if info.Mode().Perm() != 0o600 {
		return socketIdentity{}, fmt.Errorf("root socket %q has mode %04o, require 0600", path, info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return socketIdentity{}, fmt.Errorf("root socket %q: cannot read OS stat", path)
	}
	if int(stat.Uid) != euid {
		return socketIdentity{}, fmt.Errorf("root socket %q is owned by UID %d, not EUID %d", path, stat.Uid, euid)
	}
	return socketIdentity{dev: stat.Dev, ino: stat.Ino, mtimeNano: info.ModTime().UnixNano()}, nil
}

// sameIdentity returns true when the socket at path is still the recorded
// generation. Re-run the complete socket validation so a replaced path cannot
// pass merely because its inode number was recycled.
func sameIdentity(path string, id socketIdentity) bool {
	current, err := inspectRootSocket(path, os.Lstat, os.Geteuid())
	return err == nil && current == id
}
