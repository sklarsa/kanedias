package manager

import (
	"fmt"
	"sort"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// ModelSelection is the allowlisted browser wire selection for the root model.
// It carries only a model type ID and a thinking level—never a raw provider or
// model identifier.
type ModelSelection struct {
	ModelType     string `json:"modelType"`
	ThinkingLevel string `json:"thinkingLevel"`
}

// WorkerModelSelection is the allowlisted browser wire selection for one worker
// role. The role name is fixed administrator policy and the model reference is
// allowlisted against the manager catalog.
type WorkerModelSelection struct {
	WorkerType    string `json:"workerType"`
	ModelType     string `json:"modelType"`
	ThinkingLevel string `json:"thinkingLevel"`
}

// SessionLaunchRequest is the complete allowlisted root-creation request. It
// must name every configured worker role exactly once; missing, duplicate, and
// unknown roles are rejected.
type SessionLaunchRequest struct {
	Root    ModelSelection         `json:"root"`
	Workers []WorkerModelSelection `json:"workers"`
}

// ModelLaunchOption is the read-only, browser-facing view of one model type.
// Only the model type ID, label, allowed thinking levels, and default thinking
// level are exposed; the raw provider/model stay private to the catalog.
type ModelLaunchOption struct {
	ModelType            string   `json:"modelType"`
	Label                string   `json:"label"`
	ThinkingLevels       []string `json:"thinkingLevels"`
	DefaultThinkingLevel string   `json:"defaultThinkingLevel"`
}

// WorkerLaunchOption is the read-only, browser-facing view of one worker role.
// The role name and description are administrator-owned policy.
type WorkerLaunchOption struct {
	WorkerType    string `json:"workerType"`
	Description   string `json:"description"`
	ModelType     string `json:"modelType"`
	ThinkingLevel string `json:"thinkingLevel"`
}

// SessionLaunchOptions is the complete read-only launch view served to the
// browser: sorted model options, the default root selection, and sorted worker
// rows with their descriptions and default selections.
type SessionLaunchOptions struct {
	Models  []ModelLaunchOption  `json:"models"`
	Root    ModelSelection       `json:"root"`
	Workers []WorkerLaunchOption `json:"workers"`
}

// LaunchConfiguration is the immutable, allowlisted launch catalog plus the
// configured defaults. It resolves allowlisted browser requests into a
// config.SessionModelPolicy without ever exposing raw provider/model or
// description values.
type LaunchConfiguration struct {
	modelDefs   map[string]config.ModelDefinition
	modelOrder  []string
	workerDefs  map[string]config.WorkerDefaults
	workerOrder []string
	defaultRoot ModelSelection
}

// NewLaunchConfiguration derives the immutable launch catalog from a validated
// config. It resolves and validates the default root and every worker default
// so a bad default fails at construction rather than at request time.
func NewLaunchConfiguration(cfg config.Config) (LaunchConfiguration, error) {
	if len(cfg.Models) == 0 {
		return LaunchConfiguration{}, fmt.Errorf("at least one model is required")
	}

	lc := LaunchConfiguration{
		modelDefs:  make(map[string]config.ModelDefinition, len(cfg.Models)),
		modelOrder: make([]string, 0, len(cfg.Models)),
		workerDefs: make(map[string]config.WorkerDefaults, len(cfg.Workers)),
	}
	for name, def := range cfg.Models {
		lc.modelDefs[name] = def
		lc.modelOrder = append(lc.modelOrder, name)
	}
	sort.Strings(lc.modelOrder)

	// Resolve and validate the default root selection (empty thinking falls back
	// to the model-default level).
	rootProfile, err := cfg.ResolveModel(cfg.Session.ModelType, cfg.Session.ThinkingLevel)
	if err != nil {
		return LaunchConfiguration{}, fmt.Errorf("resolve default root model: %w", err)
	}
	lc.defaultRoot = ModelSelection{ModelType: cfg.Session.ModelType, ThinkingLevel: rootProfile.ThinkingLevel}

	for name, def := range cfg.Workers {
		lc.workerDefs[name] = def
		lc.workerOrder = append(lc.workerOrder, name)
	}
	sort.Strings(lc.workerOrder)

	// Validate every worker's configured default so the default launch view and
	// request are always resolvable.
	for _, name := range lc.workerOrder {
		def := lc.workerDefs[name]
		if _, err := cfg.ResolveModel(def.ModelType, def.ThinkingLevel); err != nil {
			return LaunchConfiguration{}, fmt.Errorf("worker %q default: %w", name, err)
		}
	}
	return lc, nil
}

// Resolve validates an allowlisted request against the fixed catalog and worker
// role set, then returns an independent (cloned) resolved policy.
func (lc LaunchConfiguration) Resolve(request SessionLaunchRequest) (config.SessionModelPolicy, error) {
	root, err := lc.resolveModelProfile(request.Root.ModelType, request.Root.ThinkingLevel)
	if err != nil {
		return config.SessionModelPolicy{}, contractErr(err)
	}
	if len(request.Workers) != len(lc.workerDefs) {
		return config.SessionModelPolicy{}, contractErr(fmt.Errorf("launch request must include every worker exactly once"))
	}

	policy := config.SessionModelPolicy{
		Root:    root,
		Workers: make(map[string]config.WorkerProfile, len(request.Workers)),
	}
	seen := make(map[string]struct{}, len(request.Workers))
	for _, sel := range request.Workers {
		if sel.WorkerType == "" {
			return config.SessionModelPolicy{}, contractErr(fmt.Errorf("launch request worker type is required"))
		}
		if _, dup := seen[sel.WorkerType]; dup {
			return config.SessionModelPolicy{}, contractErr(fmt.Errorf("launch request includes duplicate worker %q", sel.WorkerType))
		}
		seen[sel.WorkerType] = struct{}{}
		def, ok := lc.workerDefs[sel.WorkerType]
		if !ok {
			return config.SessionModelPolicy{}, contractErr(fmt.Errorf("launch request includes unknown worker %q", sel.WorkerType))
		}
		profile, err := lc.resolveModelProfile(sel.ModelType, sel.ThinkingLevel)
		if err != nil {
			return config.SessionModelPolicy{}, contractErr(err)
		}
		policy.Workers[sel.WorkerType] = config.WorkerProfile{
			Description:   def.Description,
			Provider:      profile.Provider,
			Model:         profile.Model,
			ThinkingLevel: profile.ThinkingLevel,
		}
	}
	for _, name := range lc.workerOrder {
		if _, ok := seen[name]; !ok {
			return config.SessionModelPolicy{}, contractErr(fmt.Errorf("launch request is missing worker %q", name))
		}
	}
	if err := policy.Validate(); err != nil {
		return config.SessionModelPolicy{}, contractErr(err)
	}
	return policy.Clone(), nil
}

// resolveModelProfile resolves an allowlisted model type ID and thinking level
// into a runtime profile using only the administrator-owned catalog values.
func (lc LaunchConfiguration) resolveModelProfile(modelType, thinkingLevel string) (config.ModelProfile, error) {
	if modelType == "" {
		return config.ModelProfile{}, fmt.Errorf("model type is required")
	}
	def, ok := lc.modelDefs[modelType]
	if !ok {
		return config.ModelProfile{}, fmt.Errorf("unknown model type %q", modelType)
	}
	if thinkingLevel == "" {
		thinkingLevel = def.DefaultThinkingLevel
	}
	supported := false
	for _, level := range def.ThinkingLevels {
		if level == thinkingLevel {
			supported = true
			break
		}
	}
	if !supported {
		return config.ModelProfile{}, fmt.Errorf("model type %q does not support thinking level %q", modelType, thinkingLevel)
	}
	return config.ModelProfile{Provider: def.Provider, Model: def.Model, ThinkingLevel: thinkingLevel}, nil
}

// LaunchOptions returns the read-only launch view with copied slices so callers
// cannot mutate the immutable catalog through the returned value.
func (lc LaunchConfiguration) LaunchOptions() SessionLaunchOptions {
	models := make([]ModelLaunchOption, 0, len(lc.modelOrder))
	for _, name := range lc.modelOrder {
		def := lc.modelDefs[name]
		models = append(models, ModelLaunchOption{
			ModelType:            name,
			Label:                def.Label,
			ThinkingLevels:       append([]string(nil), def.ThinkingLevels...),
			DefaultThinkingLevel: def.DefaultThinkingLevel,
		})
	}
	workers := make([]WorkerLaunchOption, 0, len(lc.workerOrder))
	for _, name := range lc.workerOrder {
		def := lc.workerDefs[name]
		workers = append(workers, WorkerLaunchOption{
			WorkerType:    name,
			Description:   def.Description,
			ModelType:     def.ModelType,
			ThinkingLevel: lc.workerDefaultThinking(name),
		})
	}
	return SessionLaunchOptions{Models: models, Root: lc.defaultRoot, Workers: workers}
}

// DefaultRequest returns the configured default launch request with copied
// slices.
func (lc LaunchConfiguration) DefaultRequest() SessionLaunchRequest {
	workers := make([]WorkerModelSelection, 0, len(lc.workerOrder))
	for _, name := range lc.workerOrder {
		def := lc.workerDefs[name]
		workers = append(workers, WorkerModelSelection{
			WorkerType:    name,
			ModelType:     def.ModelType,
			ThinkingLevel: lc.workerDefaultThinking(name),
		})
	}
	return SessionLaunchRequest{Root: lc.defaultRoot, Workers: workers}
}

// workerDefaultThinking returns a worker's configured default thinking level,
// falling back to the referenced model's default when the worker does not
// specify one.
func (lc LaunchConfiguration) workerDefaultThinking(name string) string {
	def := lc.workerDefs[name]
	if def.ThinkingLevel != "" {
		return def.ThinkingLevel
	}
	if model, ok := lc.modelDefs[def.ModelType]; ok {
		return model.DefaultThinkingLevel
	}
	return ""
}

// contractErr wraps a validation failure as a contract.InvalidRequest error.
func contractErr(err error) error {
	return contract.NewError(contract.ErrorInvalidRequest, err.Error())
}
