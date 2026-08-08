package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSupervisorEventLimitsPreserveOmissionAndZero(t *testing.T) {
	zero := 0
	tests := []struct {
		name string
		cfg  SupervisorEventsConfig
		want EventLimits
		err  string
	}{
		{"omitted", SupervisorEventsConfig{}, EventLimits{4096, 16 << 20}, ""},
		{"disable count", SupervisorEventsConfig{MaxEvents: &zero}, EventLimits{0, 16 << 20}, ""},
		{"disable bytes", SupervisorEventsConfig{MaxBytes: &zero}, EventLimits{4096, 0}, ""},
		{"disable both", SupervisorEventsConfig{MaxEvents: &zero, MaxBytes: &zero}, EventLimits{}, "at least one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.Limits()
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Limits = %#v, %v", got, err)
			}
		})
	}
}

func TestSupervisorEventLimitsRejectsNegative(t *testing.T) {
	neg := -1
	tests := []struct {
		name string
		cfg  SupervisorEventsConfig
	}{
		{"negative max_events", SupervisorEventsConfig{MaxEvents: &neg}},
		{"negative max_bytes", SupervisorEventsConfig{MaxBytes: &neg}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.cfg.Limits()
			if err == nil {
				t.Fatalf("Limits() error = nil, want rejection of negative value")
			}
		})
	}
}

func TestServerConfigResolveParsesQuotedDurations(t *testing.T) {
	content := `[network]
ipv4 = "10.76.111.1/24"
[base_image]
name = "sandbox"
source = "images:"
image = "debian/13"

[server]
discovery_interval = "5s"
snapshot_interval = "1s"
spawn_timeout = "2m"
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	resolved, err := cfg.Server.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.DiscoveryInterval != 5*time.Second {
		t.Errorf("DiscoveryInterval = %v, want 5s", resolved.DiscoveryInterval)
	}
	if resolved.SnapshotInterval != time.Second {
		t.Errorf("SnapshotInterval = %v, want 1s", resolved.SnapshotInterval)
	}
	if resolved.SpawnTimeout != 2*time.Minute {
		t.Errorf("SpawnTimeout = %v, want 2m", resolved.SpawnTimeout)
	}
}

func TestServerConfigResolveDefaultDurations(t *testing.T) {
	var cfg ServerConfig
	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.DiscoveryInterval != DefaultServerDiscoveryInterval {
		t.Errorf("DiscoveryInterval = %v, want %v", resolved.DiscoveryInterval, DefaultServerDiscoveryInterval)
	}
	if resolved.SnapshotInterval != DefaultServerSnapshotInterval {
		t.Errorf("SnapshotInterval = %v, want %v", resolved.SnapshotInterval, DefaultServerSnapshotInterval)
	}
	if resolved.SpawnTimeout != DefaultServerSpawnTimeout {
		t.Errorf("SpawnTimeout = %v, want %v", resolved.SpawnTimeout, DefaultServerSpawnTimeout)
	}
}

func TestServerConfigResolveRejectsZeroAndNegativeDurations(t *testing.T) {
	zero := Duration{0}
	negative := Duration{-time.Second}
	tests := []struct {
		name string
		cfg  ServerConfig
	}{
		{"zero discovery", ServerConfig{DiscoveryInterval: &zero}},
		{"negative discovery", ServerConfig{DiscoveryInterval: &negative}},
		{"zero snapshot", ServerConfig{SnapshotInterval: &zero}},
		{"zero spawn", ServerConfig{SpawnTimeout: &zero}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.cfg.Resolve()
			if err == nil {
				t.Fatalf("Resolve() error = nil, want rejection of zero/negative duration")
			}
		})
	}
}

func TestValidateSupervisorCallsEventLimits(t *testing.T) {
	zero := 0
	cfg := Config{
		BaseImage: BaseImage{Name: "sandbox", Source: "images:", Image: "debian/13"},
		Workers:   map[string]WorkerProfile{"worker": {Description: "work", Provider: "p", Model: "m"}},
		Supervisor: SupervisorConfig{Events: SupervisorEventsConfig{
			MaxEvents: &zero, MaxBytes: &zero,
		}},
	}
	err := cfg.ValidateSupervisor()
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("ValidateSupervisor() error = %v, want event-limits error", err)
	}
}

func TestSupervisorEventsConfigTOML(t *testing.T) {
	content := `[network]
ipv4 = "10.76.111.1/24"
[base_image]
name = "sandbox"
source = "images:"
image = "debian/13"
[workers.worker]
description = "work"
provider = "provider"
model = "model"

[supervisor.events]
max_events = 7
max_bytes = 1024
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	limits, err := cfg.Supervisor.Events.Limits()
	if err != nil {
		t.Fatalf("Limits() error = %v", err)
	}
	if limits != (EventLimits{MaxEvents: 7, MaxBytes: 1024}) {
		t.Fatalf("Limits = %#v, want {7, 1024}", limits)
	}
}
