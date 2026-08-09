package config

import (
	"fmt"
	"strings"
	"time"
)

const (
	DefaultServerDiscoveryInterval  = 5 * time.Second
	DefaultServerSnapshotInterval   = time.Second
	DefaultServerSpawnTimeout       = 2 * time.Minute
	DefaultSupervisorEventMaxEvents = 4_096
	DefaultSupervisorEventMaxBytes  = 16 << 20
)

// Duration is a time.Duration that unmarshals from a quoted TOML string such
// as "5s". Wrap in a pointer to distinguish omission from an explicit zero.
type Duration struct{ time.Duration }

// UnmarshalText parses a Go duration string.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", text, err)
	}
	d.Duration = parsed
	return nil
}

// ServerConfig holds raw TOML server settings. Pointer durations distinguish
// omission from an explicit zero; managers resolve empty paths to defaults.
type ServerConfig struct {
	Hostname          string    `toml:"hostname"`
	RootSocketDir     string    `toml:"root_socket_dir"`
	SessionLogDir     string    `toml:"session_log_dir"`
	DiscoveryInterval *Duration `toml:"discovery_interval"`
	SnapshotInterval  *Duration `toml:"snapshot_interval"`
	SpawnTimeout      *Duration `toml:"spawn_timeout"`
	SessionBinary     string    `toml:"session_binary"`
	RequireSession    *bool     `toml:"require_session"`
}

// SupervisorConfig holds supervisor-wide settings.
type SupervisorConfig struct {
	Events SupervisorEventsConfig `toml:"events"`
}

// SupervisorEventsConfig configures root event replay retention. Nil fields
// fall back to defaults; an explicit zero disables that limit.
type SupervisorEventsConfig struct {
	MaxEvents *int `toml:"max_events"`
	MaxBytes  *int `toml:"max_bytes"`
}

// EventLimits are the resolved, independent supervisor replay limits. Zero
// disables the corresponding limit.
type EventLimits struct {
	MaxEvents int
	MaxBytes  int
}

// Limits resolves configured event limits against the defaults. At least one
// limit must remain enabled; negative values are rejected.
func (c SupervisorEventsConfig) Limits() (EventLimits, error) {
	limits := EventLimits{
		MaxEvents: DefaultSupervisorEventMaxEvents,
		MaxBytes:  DefaultSupervisorEventMaxBytes,
	}
	if c.MaxEvents != nil {
		if *c.MaxEvents < 0 {
			return EventLimits{}, fmt.Errorf("supervisor events max_events must be >= 0")
		}
		limits.MaxEvents = *c.MaxEvents
	}
	if c.MaxBytes != nil {
		if *c.MaxBytes < 0 {
			return EventLimits{}, fmt.Errorf("supervisor events max_bytes must be >= 0")
		}
		limits.MaxBytes = *c.MaxBytes
	}
	if limits.MaxEvents == 0 && limits.MaxBytes == 0 {
		return EventLimits{}, fmt.Errorf("supervisor events require at least one of max_events or max_bytes")
	}
	return limits, nil
}

// ResolvedServerConfig contains fully-merged server settings. Path and binary
// fields stay empty until manager path resolution.
type ResolvedServerConfig struct {
	Hostname          string
	RootSocketDir     string
	SessionLogDir     string
	DiscoveryInterval time.Duration
	SnapshotInterval  time.Duration
	SpawnTimeout      time.Duration
	SessionBinary     string
	RequireSession    bool
}

func validateServerHostname(hostname string) error {
	if hostname == "" {
		return nil
	}
	if len(hostname) > 253 || strings.ContainsAny(hostname, ":/\\?#[]@ \t\r\n") {
		return fmt.Errorf("server hostname %q must be a plain DNS hostname", hostname)
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("server hostname %q contains an invalid DNS label", hostname)
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
				(char < '0' || char > '9') && char != '-' {
				return fmt.Errorf("server hostname %q contains an invalid DNS label", hostname)
			}
		}
	}
	return nil
}

// Resolve applies duration defaults and rejects invalid hostnames and zero or negative intervals.
func (c ServerConfig) Resolve() (ResolvedServerConfig, error) {
	if err := validateServerHostname(c.Hostname); err != nil {
		return ResolvedServerConfig{}, err
	}
	requireSession := false
	if c.RequireSession != nil {
		requireSession = *c.RequireSession
	}
	resolved := ResolvedServerConfig{
		Hostname:          c.Hostname,
		RootSocketDir:     c.RootSocketDir,
		SessionLogDir:     c.SessionLogDir,
		SessionBinary:     c.SessionBinary,
		DiscoveryInterval: DefaultServerDiscoveryInterval,
		SnapshotInterval:  DefaultServerSnapshotInterval,
		SpawnTimeout:      DefaultServerSpawnTimeout,
		RequireSession:    requireSession,
	}
	if c.DiscoveryInterval != nil {
		resolved.DiscoveryInterval = c.DiscoveryInterval.Duration
	}
	if c.SnapshotInterval != nil {
		resolved.SnapshotInterval = c.SnapshotInterval.Duration
	}
	if c.SpawnTimeout != nil {
		resolved.SpawnTimeout = c.SpawnTimeout.Duration
	}
	for name, value := range map[string]time.Duration{
		"discovery_interval": resolved.DiscoveryInterval,
		"snapshot_interval":  resolved.SnapshotInterval,
		"spawn_timeout":      resolved.SpawnTimeout,
	} {
		if value <= 0 {
			return ResolvedServerConfig{}, fmt.Errorf("server %s must be positive", name)
		}
	}
	return resolved, nil
}
