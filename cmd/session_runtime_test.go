package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

func TestInheritedChildPolicySelectsExactWorkerAndClonesMap(t *testing.T) {
	bootstrap := process.Bootstrap{
		Policy: config.SessionModelPolicy{
			Root: config.ModelProfile{Provider: "root-provider", Model: "root-model", ThinkingLevel: "off"},
			Workers: map[string]config.WorkerProfile{
				"worker":   {Description: "work", Provider: "admitted-provider", Model: "admitted-model", ThinkingLevel: "xhigh"},
				"reviewer": {Description: "review", Provider: "review-provider", Model: "review-model", ThinkingLevel: "medium"},
			},
		},
		Request: contract.CreateChildRequest{WorkerType: "worker"},
	}
	policy, worker, err := inheritedChildPolicy(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if want := bootstrap.Policy.Workers["worker"]; worker != want {
		t.Fatalf("selected worker = %#v, want exact inherited profile %#v", worker, want)
	}
	policy.Workers["worker"] = config.WorkerProfile{Description: "mutated", Provider: "mutated", Model: "mutated", ThinkingLevel: "off"}
	if reflect.DeepEqual(policy, bootstrap.Policy) {
		t.Fatal("resolved child policy aliases bootstrap policy")
	}
	if len(policy.Workers) != 2 || len(bootstrap.Policy.Workers) != 2 {
		t.Fatalf("worker roles lost: resolved=%#v bootstrap=%#v", policy.Workers, bootstrap.Policy.Workers)
	}
}

func TestProductionChildRunnerUsesInheritedPolicyDespiteChangedGlobalDefaults(t *testing.T) {
	content := `[network]
ipv4 = "10.76.111.1/24"
[base_image]
name = "sandbox"
source = "images:"
image = "debian/13"
[models.changed-default]
provider = "changed-provider"
model = "changed-model"
thinking_levels = ["low"]
default_thinking_level = "low"
[session]
model_type = "changed-default"
thinking_level = "low"
[workers.worker]
description = "changed global worker"
model_type = "changed-default"

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
		SessionID: "session-child",
		ParentID:  "session-parent",
		RootID:    "session-root",
		Request: contract.CreateChildRequest{
			Kind:       contract.ChildKindRead,
			Context:    contract.ContextFresh,
			WorkerType: "worker",
			Task:       "test",
		},
		Policy: config.SessionModelPolicy{
			Root: config.ModelProfile{Provider: "admitted-root-provider", Model: "admitted-root-model", ThinkingLevel: "off"},
			Workers: map[string]config.WorkerProfile{
				"worker":   {Description: "admitted worker", Provider: "admitted-provider", Model: "admitted-model", ThinkingLevel: "xhigh"},
				"reviewer": {Description: "admitted reviewer", Provider: "review-provider", Model: "review-model", ThinkingLevel: "medium"},
			},
		},
		SourceInstance: "session-session-parent",
		SourceVolume:   "workspace-session-parent",
		SocketPath:     filepath.Join(t.TempDir(), "child.sock"),
	}

	// The sentinel is reached only after inherited-policy validation and worker
	// resolution, but before any provider/Incus side effect.
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
