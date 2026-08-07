package incusworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sklarsa/kanedias/internal/incusclient"
)

const (
	storagePoolsPath = "/var/lib/incus/storage-pools"
	disksPath        = "/var/lib/incus/disks"
)

type Executor interface {
	Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)
}

type innerStoragePool struct {
	Driver string            `json:"driver"`
	Config map[string]string `json:"config"`
}

func WaitReady(ctx context.Context, executor Executor, instance string, timeout time.Duration) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := []string{"incus", "admin", "waitready", "--timeout", strconv.Itoa(int(timeout.Seconds()))}
	_, stderr, err := executor.Exec(readyCtx, instance, incusclient.ExecRequest{Command: command})
	if err != nil {
		return fmt.Errorf("wait for nested Incus in instance %q (stderr %q): %w", instance, strings.TrimSpace(stderr), err)
	}
	return nil
}

func VerifyNativeBtrfs(ctx context.Context, executor Executor, instance string) error {
	stdout, stderr, err := executor.Exec(ctx, instance, incusclient.ExecRequest{
		Command: []string{"incus", "query", "/1.0/storage-pools/default"},
	})
	if err != nil {
		return fmt.Errorf("query nested Incus storage pool in instance %q (stderr %q): %w", instance, strings.TrimSpace(stderr), err)
	}

	var pool innerStoragePool
	if err := json.Unmarshal([]byte(stdout), &pool); err != nil {
		return fmt.Errorf("decode nested Incus storage pool: %w", err)
	}
	if pool.Driver != "btrfs" {
		return fmt.Errorf("nested Incus storage pool driver is %q, want %q", pool.Driver, "btrfs")
	}

	source := filepath.Clean(pool.Config["source"])
	if pathAtOrBelow(source, disksPath) || strings.HasSuffix(source, ".img") {
		return fmt.Errorf("nested Incus storage pool source %q is not native btrfs storage", source)
	}
	if !pathStrictlyBelow(source, storagePoolsPath) {
		return fmt.Errorf("nested Incus storage pool source %q is not below %q", source, storagePoolsPath)
	}
	return nil
}

func initialize(ctx context.Context, executor Executor, instance string, newSeed bool, timeout time.Duration) error {
	if err := WaitReady(ctx, executor, instance, timeout); err != nil {
		return err
	}
	if newSeed {
		_, stderr, err := executor.Exec(ctx, instance, incusclient.ExecRequest{
			Command: []string{"incus", "admin", "init", "--minimal"},
		})
		if err != nil {
			return fmt.Errorf("initialize nested Incus in instance %q (stderr %q): %w", instance, strings.TrimSpace(stderr), err)
		}
		if err := WaitReady(ctx, executor, instance, timeout); err != nil {
			return err
		}
	}
	return VerifyNativeBtrfs(ctx, executor, instance)
}

func syncImages(ctx context.Context, executor Executor, instance string, images []string) error {
	for _, image := range images {
		_, stderr, err := executor.Exec(ctx, instance, incusclient.ExecRequest{
			Command: []string{"incus", "image", "copy", image, "local:", "--copy-aliases", "--auto-update", "--reuse"},
		})
		if err != nil {
			return fmt.Errorf("copy image %q into nested Incus in instance %q (stderr %q): %w", image, instance, strings.TrimSpace(stderr), err)
		}
	}
	return nil
}

func quiesce(ctx context.Context, executor Executor, instance string) error {
	for _, command := range [][]string{
		{"systemctl", "stop", "incus.socket"},
		{"systemctl", "stop", "incus.service"},
	} {
		_, stderr, err := executor.Exec(ctx, instance, incusclient.ExecRequest{Command: command})
		if err != nil {
			return fmt.Errorf("run %q in instance %q (stderr %q): %w", strings.Join(command, " "), instance, strings.TrimSpace(stderr), err)
		}
	}

	values := make([]string, 0, 3)
	for _, command := range [][]string{
		{"systemctl", "show", "--property=ActiveState", "--value", "incus.socket"},
		{"systemctl", "show", "--property=ActiveState", "--value", "incus.service"},
		{"systemctl", "show", "--property=MainPID", "--value", "incus.service"},
	} {
		stdout, stderr, err := executor.Exec(ctx, instance, incusclient.ExecRequest{Command: command})
		if err != nil {
			return fmt.Errorf("run %q in instance %q (stderr %q): %w", strings.Join(command, " "), instance, strings.TrimSpace(stderr), err)
		}
		values = append(values, strings.TrimSpace(stdout))
	}

	if values[0] != "inactive" {
		return fmt.Errorf("nested Incus socket is %q after stop, want %q", values[0], "inactive")
	}
	if values[1] != "inactive" {
		return fmt.Errorf("nested Incus service is %q after stop, want %q", values[1], "inactive")
	}
	if values[2] != "0" {
		return fmt.Errorf("nested Incus service MainPID is %q after stop, want %q", values[2], "0")
	}
	return nil
}

func pathStrictlyBelow(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func pathAtOrBelow(path, parent string) bool {
	return path == parent || pathStrictlyBelow(path, parent)
}
