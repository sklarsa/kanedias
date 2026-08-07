//go:build incus

package incusworkspace_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/sandbox"
	incusworkspace "github.com/sklarsa/kanedias/internal/workspace/incus"
)

type cleanupFailures struct {
	mu     sync.Mutex
	errors []error
}

func (failures *cleanupFailures) add(err error) {
	if err == nil {
		return
	}
	failures.mu.Lock()
	defer failures.mu.Unlock()
	failures.errors = append(failures.errors, err)
}

func (failures *cleanupFailures) joined() error {
	failures.mu.Lock()
	defer failures.mu.Unlock()
	return errors.Join(failures.errors...)
}

func TestLiveNativeNestedIncusIsolation(t *testing.T) {
	if os.Getenv("KANEDIAS_LIVE_NESTED_INCUS") != "1" {
		t.Skip("set KANEDIAS_LIVE_NESTED_INCUS=1 to run the native nested Incus test")
	}

	configPath := os.Getenv("KANEDIAS_CONFIG")
	if configPath == "" {
		configPath = "./config.toml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	var cleanup cleanupFailures
	t.Cleanup(func() {
		if err := cleanup.joined(); err != nil {
			t.Errorf("clean up live nested Incus test: %v", err)
		}
	})

	outer, err := incusclient.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(outer.Disconnect)

	pool, err := outer.ResolvePool(ctx, cfg.Workspace.Pool)
	if err != nil {
		t.Fatal(err)
	}
	storagePool, err := outer.GetStoragePool(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if storagePool.Driver != "btrfs" {
		t.Skipf("outer Incus storage pool %q uses %q, need btrfs for native nested Incus", pool, storagePool.Driver)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	cfg.Workspace.Volume = "kanedias-live-repos-" + suffix
	cfg.Workspace.Repos = nil
	cfg.Workspace.Incus.Volume = "kanedias-live-incus-seed-" + suffix
	cfg.Workspace.Incus.Images = []string{"images:debian/13"}

	if err := outer.CreateStorageVolume(ctx, pool, cfg.Workspace.Volume); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := outer.DeleteStorageVolume(cleanupCtx, pool, cfg.Workspace.Volume); err != nil {
			cleanup.add(fmt.Errorf("delete repository seed %q: %w", cfg.Workspace.Volume, err))
		}
	})

	if err := incusworkspace.Sync(ctx, cfg, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := outer.DeleteStorageVolume(cleanupCtx, pool, cfg.Workspace.Incus.Volume); err != nil {
			cleanup.add(fmt.Errorf("delete nested Incus seed %q: %w", cfg.Workspace.Incus.Volume, err))
		}
	})

	sandboxA := "kanedias-live-a-" + suffix
	sandboxB := "kanedias-live-b-" + suffix
	type createResult struct {
		name string
		err  error
	}
	results := make(chan createResult, 2)
	for _, name := range []string{sandboxA, sandboxB} {
		name := name
		go func() {
			err := sandbox.Create(ctx, cfg, name, os.Stdout, os.Stderr)
			if err == nil {
				t.Cleanup(func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					if err := sandbox.Destroy(cleanupCtx, cfg, name, io.Discard, io.Discard); err != nil {
						cleanup.add(fmt.Errorf("destroy outer sandbox %q: %w", name, err))
					}
				})
			}
			results <- createResult{name: name, err: err}
		}()
	}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Errorf("create outer sandbox %q: %v", result.name, result.err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	prepareNestedSandbox(t, ctx, outer, sandboxA, "inner-a", "sandbox-a", &cleanup)
	prepareNestedSandbox(t, ctx, outer, sandboxB, "inner-b", "sandbox-b", &cleanup)
	assertInnerAbsent(t, ctx, outer, sandboxA, "inner-b")
	assertInnerAbsent(t, ctx, outer, sandboxB, "inner-a")
}

func prepareNestedSandbox(t *testing.T, ctx context.Context, outer *incusclient.Client, sandboxName, innerName, marker string, cleanup *cleanupFailures) {
	t.Helper()

	if err := incusworkspace.VerifyNativeBtrfs(ctx, outer, sandboxName); err != nil {
		t.Fatal(err)
	}
	aliases := liveExec(t, ctx, outer, sandboxName, "incus", "image", "list", "--format", "csv", "-c", "l")
	if !strings.Contains(aliases, "debian/13") {
		t.Fatalf("nested Incus image aliases in %q = %q, want debian/13", sandboxName, strings.TrimSpace(aliases))
	}

	liveExec(t, ctx, outer, sandboxName, "incus", "launch", "debian/13", innerName)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, stderr, err := outer.Exec(cleanupCtx, sandboxName, incusclient.ExecRequest{
			Command: []string{"incus", "delete", "--force", innerName},
		})
		if err != nil {
			cleanup.add(fmt.Errorf("delete inner instance %q in %q (stderr %q): %w", innerName, sandboxName, strings.TrimSpace(stderr), err))
		}
	})

	liveExec(t, ctx, outer, sandboxName, "incus", "exec", innerName, "--", "sh", "-c", "printf "+marker+" >/root/kanedias-marker")
	gotMarker := strings.TrimSpace(liveExec(t, ctx, outer, sandboxName, "incus", "exec", innerName, "--", "cat", "/root/kanedias-marker"))
	if gotMarker != marker {
		t.Fatalf("marker in %s/%s = %q, want %q", sandboxName, innerName, gotMarker, marker)
	}
}

func assertInnerAbsent(t *testing.T, ctx context.Context, outer *incusclient.Client, sandboxName, absentName string) {
	t.Helper()
	got := strings.TrimSpace(liveExec(t, ctx, outer, sandboxName, "incus", "list", absentName, "--format", "csv", "-c", "n"))
	if got != "" {
		t.Fatalf("isolated sandbox %q can see inner instance %q: name column = %q", sandboxName, absentName, got)
	}
}

func liveExec(t *testing.T, ctx context.Context, outer *incusclient.Client, sandboxName string, command ...string) string {
	t.Helper()
	stdout, stderr, err := outer.Exec(ctx, sandboxName, incusclient.ExecRequest{Command: command})
	if err != nil {
		t.Fatalf("run %q in outer sandbox %q (stderr %q): %v", strings.Join(command, " "), sandboxName, strings.TrimSpace(stderr), err)
	}
	return stdout
}
