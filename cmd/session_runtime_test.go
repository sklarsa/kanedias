package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
)

func validSupervisorConfig() config.Config {
	return config.Config{
		BaseImage: config.BaseImage{Name: "sandbox", Source: "images:", Image: "debian/13"},
		Models: map[string]config.ModelDefinition{
			"gpt-5-6-sol": {
				Provider:             "openai-codex",
				Model:                "gpt-5.6-sol",
				ThinkingLevels:       []string{"low", "high"},
				DefaultThinkingLevel: "high",
			},
		},
		Session: config.SessionDefaults{ModelType: "gpt-5-6-sol", ThinkingLevel: "high"},
		Workers: map[string]config.WorkerDefaults{"worker": {
			Description: "work", ModelType: "gpt-5-6-sol",
		}},
	}
}

func TestRunSupervisorSelectsConfiguredEventLimitsBeforeProvisioning(t *testing.T) {
	maxEvents, maxBytes := 7, 1024
	cfg := validSupervisorConfig()
	cfg.Supervisor.Events = config.SupervisorEventsConfig{MaxEvents: &maxEvents, MaxBytes: &maxBytes}
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("broker sentinel")
	err = runSupervisorWithBrokerFactory(context.Background(), cfg, SessionOptions{
		SocketPath: filepath.Join(t.TempDir(), "root.sock"), ConfigPath: "/tmp/config.toml", Policy: policy,
	}, io.Discard, func(got supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		if got != (supervisor.EventBrokerOptions{MaxEvents: 7, MaxBytes: 1024}) {
			t.Fatalf("options = %#v", got)
		}
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v", err)
	}
}

func TestRunSupervisorDefaultEventLimitsWhenUnconfigured(t *testing.T) {
	cfg := validSupervisorConfig()
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("broker sentinel")
	err = runSupervisorWithBrokerFactory(context.Background(), cfg, SessionOptions{
		SocketPath: filepath.Join(t.TempDir(), "root.sock"), ConfigPath: "/tmp/config.toml", Policy: policy,
	}, io.Discard, func(got supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		want := supervisor.EventBrokerOptions{
			MaxEvents: config.DefaultSupervisorEventMaxEvents,
			MaxBytes:  config.DefaultSupervisorEventMaxBytes,
		}
		if got != want {
			t.Fatalf("options = %#v, want %#v", got, want)
		}
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v", err)
	}
}

func TestRunSupervisorRejectsInvalidConfig(t *testing.T) {
	called := false
	policy, err := validSupervisorConfig().DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	err = runSupervisorWithBrokerFactory(context.Background(), config.Config{}, SessionOptions{
		SocketPath: filepath.Join(t.TempDir(), "root.sock"), ConfigPath: "/tmp/config.toml", Policy: policy,
	}, io.Discard, func(supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		called = true
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if called {
		t.Fatal("broker factory called before config validation")
	}
}

func TestRunSupervisorRejectsInvalidPolicyBeforeConfigAndBroker(t *testing.T) {
	called := false
	err := runSupervisorWithBrokerFactory(context.Background(), config.Config{}, SessionOptions{
		SocketPath: filepath.Join(t.TempDir(), "root.sock"), ConfigPath: "/tmp/config.toml",
		Policy: config.SessionModelPolicy{Root: config.ModelProfile{Model: "model", ThinkingLevel: "high"}},
	}, io.Discard, func(supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		called = true
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("policy validation error = %v", err)
	}
	if called {
		t.Fatal("broker factory called before policy validation")
	}
}

func TestProductionChildRunnerSelectsConfiguredEventLimitsBeforeProvisioning(t *testing.T) {
	content := `[network]
ipv4 = "10.76.111.1/24"
[base_image]
name = "sandbox"
source = "images:"
image = "debian/13"
[models.gpt-5-6-sol]
provider = "openai-codex"
model = "gpt-5.6-sol"
thinking_levels = ["high"]
default_thinking_level = "high"
[session]
model_type = "gpt-5-6-sol"
thinking_level = "high"
[workers.worker]
description = "work"
model_type = "gpt-5-6-sol"

[supervisor.events]
max_events = 7
max_bytes = 1024
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("KANEDIAS_CONFIG", path)

	sentinel := errors.New("broker sentinel")
	bootstrap := process.Bootstrap{
		SessionID: "session-test",
		ParentID:  "session-test",
		RootID:    "session-test",
		Request: contract.CreateChildRequest{
			Kind:       contract.ChildKindRead,
			Context:    contract.ContextFresh,
			WorkerType: "worker",
			Task:       "test",
		},
		Worker: config.WorkerProfile{
			Description: "work", Provider: "provider", Model: "model",
		},
		SocketPath: filepath.Join(t.TempDir(), "child.sock"),
	}

	err := productionChildRunnerWithBrokerFactory(context.Background(), bootstrap, nil, func(got supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		if got != (supervisor.EventBrokerOptions{MaxEvents: 7, MaxBytes: 1024}) {
			t.Fatalf("options = %#v", got)
		}
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v", err)
	}
}
