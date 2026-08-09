package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ModelProfile is the canonical, credential-free runtime model selection that
// every session, child, and bootstrap carries. It is what Pi is actually
// launched with and what effective-model binding compares against.
type ModelProfile struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinkingLevel"`
}

// WorkerProfile is the canonical, credential-free resolved runtime profile for
// a single worker type.
type WorkerProfile struct {
	Description   string `json:"description"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinkingLevel"`
}

// SessionModelPolicy is the complete, resolved model policy owned by one
// session. Every node holds a cloned, immutable copy and passes it unchanged
// to descendants.
type SessionModelPolicy struct {
	Root    ModelProfile             `json:"root"`
	Workers map[string]WorkerProfile `json:"workers"`
}

// Clone returns a deep copy of the workers map so callers can mutate the copy
// without aliasing the original policy.
func (p SessionModelPolicy) Clone() SessionModelPolicy {
	out := p
	out.Workers = make(map[string]WorkerProfile, len(p.Workers))
	for name, worker := range p.Workers {
		out.Workers[name] = worker
	}
	return out
}

// WorkerNames returns the worker names in sorted order.
func (p SessionModelPolicy) WorkerNames() []string {
	names := make([]string, 0, len(p.Workers))
	for name := range p.Workers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolveWorker returns the resolved profile for a named worker.
func (p SessionModelPolicy) ResolveWorker(name string) (WorkerProfile, error) {
	worker, ok := p.Workers[name]
	if !ok {
		return WorkerProfile{}, fmt.Errorf("unknown worker type %q", name)
	}
	return worker, nil
}

// Validate checks the structural invariants of the resolved policy: a nonempty
// root provider/model with a valid thinking level, nonempty descriptions and
// provider/model pairs for every worker, valid thinking levels, and at least
// one worker.
func (p SessionModelPolicy) Validate() error {
	if strings.TrimSpace(p.Root.Provider) == "" || strings.TrimSpace(p.Root.Model) == "" {
		return fmt.Errorf("root provider and model are required")
	}
	if !validThinkingLevel(p.Root.ThinkingLevel) {
		return fmt.Errorf("root thinking level %q is invalid", p.Root.ThinkingLevel)
	}
	if len(p.Workers) == 0 {
		return fmt.Errorf("at least one worker is required")
	}
	for _, name := range p.WorkerNames() {
		worker := p.Workers[name]
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("worker name is required")
		}
		if strings.TrimSpace(worker.Description) == "" {
			return fmt.Errorf("worker %q description is required", name)
		}
		if strings.TrimSpace(worker.Provider) == "" || strings.TrimSpace(worker.Model) == "" {
			return fmt.Errorf("worker %q provider and model are required", name)
		}
		if !validThinkingLevel(worker.ThinkingLevel) {
			return fmt.Errorf("worker %q thinking level %q is invalid", name, worker.ThinkingLevel)
		}
	}
	return nil
}

// ModelDefinition is the raw TOML catalog entry for one model type. Workers
// and the session reference provider/model selections by a model type ID.
type ModelDefinition struct {
	Label                string   `toml:"label"`
	Provider             string   `toml:"provider"`
	Model                string   `toml:"model"`
	ThinkingLevels       []string `toml:"thinking_levels"`
	DefaultThinkingLevel string   `toml:"default_thinking_level"`
}

// SessionDefaults selects the root model/thinking that a fresh session starts
// Pi with when no explicit request supplies a model type.
type SessionDefaults struct {
	ModelType     string `toml:"model_type"`
	ThinkingLevel string `toml:"thinking_level"`
}

// WorkerDefaults is the raw TOML worker configuration. It references a model
// type rather than embedding a raw provider/model pair.
type WorkerDefaults struct {
	Description   string `toml:"description"`
	ModelType     string `toml:"model_type"`
	ThinkingLevel string `toml:"thinking_level"`
}

var modelTypeSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// validThinkingLevel reports whether level is one of the global thinking
// levels understood by the runtime.
func validThinkingLevel(level string) bool {
	_, ok := validThinkingLevels[level]
	return ok
}

// ResolveModel resolves a model type ID and an optional thinking level into a
// runtime ModelProfile. An empty thinkingLevel falls back to the model's
// configured default. The requested level must be supported by the model.
func (cfg Config) ResolveModel(modelType, thinkingLevel string) (ModelProfile, error) {
	def, ok := cfg.Models[modelType]
	if !ok {
		return ModelProfile{}, fmt.Errorf("unknown model type %q", modelType)
	}
	if thinkingLevel == "" {
		thinkingLevel = def.DefaultThinkingLevel
	}
	if !contains(def.ThinkingLevels, thinkingLevel) {
		return ModelProfile{}, fmt.Errorf("model type %q does not support thinking level %q", modelType, thinkingLevel)
	}
	return ModelProfile{Provider: def.Provider, Model: def.Model, ThinkingLevel: thinkingLevel}, nil
}

// DefaultSessionModelPolicy resolves the session default root model and every
// configured worker into one SessionModelPolicy.
func (cfg Config) DefaultSessionModelPolicy() (SessionModelPolicy, error) {
	root, err := cfg.ResolveModel(cfg.Session.ModelType, cfg.Session.ThinkingLevel)
	if err != nil {
		return SessionModelPolicy{}, err
	}
	policy := SessionModelPolicy{Root: root, Workers: make(map[string]WorkerProfile, len(cfg.Workers))}
	for _, name := range cfg.WorkerNames() {
		defaults := cfg.Workers[name]
		profile, err := cfg.ResolveModel(defaults.ModelType, defaults.ThinkingLevel)
		if err != nil {
			return SessionModelPolicy{}, fmt.Errorf("worker %q: %w", name, err)
		}
		if strings.TrimSpace(defaults.Description) == "" {
			return SessionModelPolicy{}, fmt.Errorf("worker %q description is required", name)
		}
		policy.Workers[name] = WorkerProfile{
			Description:   defaults.Description,
			Provider:      profile.Provider,
			Model:         profile.Model,
			ThinkingLevel: profile.ThinkingLevel,
		}
	}
	if err := policy.Validate(); err != nil {
		return SessionModelPolicy{}, err
	}
	return policy, nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
