package incusworkspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedLockAllowsReadersAndExcludesWriter(t *testing.T) {
	pool := "pool-" + strings.ReplaceAll(t.Name(), "/", "-")
	first, err := acquireSeedLock(pool, "seed", false)
	if err != nil {
		t.Fatal(err)
	}

	second, err := acquireSeedLock(pool, "seed", false)
	if err != nil {
		t.Fatal(err)
	}

	if writer, err := acquireSeedLock(pool, "seed", true); err == nil {
		_ = writer.Close()
		t.Fatal("exclusive seed lock succeeded while shared locks were held")
	}

	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	writer, err := acquireSeedLock(pool, "seed", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
}

func TestSeedLockUsesPrivatePermissions(t *testing.T) {
	pool := "pool-" + strings.ReplaceAll(t.Name(), "/", "-")
	seed := "seed"
	lock, err := acquireSeedLock(pool, seed, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()

	dir := filepath.Join(os.TempDir(), fmt.Sprintf("kanedias-incus-seed-locks-%d", os.Getuid()))
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("lock directory mode = %04o, want 0700", got)
	}

	digest := sha256.Sum256([]byte(pool + "\x00" + seed))
	path := filepath.Join(dir, hex.EncodeToString(digest[:])+".lock")
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock file mode = %04o, want 0600", got)
	}
}
