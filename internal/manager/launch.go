package manager

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

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
// unknown roles are rejected. Name is optional presentation metadata and
// Repository is either empty or an exact configured owner/repository slug.
type SessionLaunchRequest struct {
	Name       string                 `json:"name"`
	Repository string                 `json:"repository"`
	Root       ModelSelection         `json:"root"`
	Workers    []WorkerModelSelection `json:"workers"`
}

// RepositoryLaunchOption is the read-only, browser-facing view of one
// configured workspace repository. It exposes only the slug—never a URL,
// filesystem path, credential, or arbitrary clone source.
type RepositoryLaunchOption struct {
	Slug string `json:"slug"`
}

// ResolvedSessionLaunch is the immutable, validated result of resolving an
// allowlisted launch request: a normalized optional name, the selected
// workspace start, and the resolved model policy.
type ResolvedSessionLaunch struct {
	Name      string
	Workspace config.WorkspaceStart
	Policy    config.SessionModelPolicy
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
// browser: sorted model options, the default root selection, sorted worker
// rows with their descriptions and default selections, and the sorted
// configured repository slugs.
type SessionLaunchOptions struct {
	Models       []ModelLaunchOption      `json:"models"`
	Root         ModelSelection           `json:"root"`
	Workers      []WorkerLaunchOption     `json:"workers"`
	Repositories []RepositoryLaunchOption `json:"repositories"`
}

// LaunchConfiguration is the immutable, allowlisted launch catalog plus the
// configured defaults. It resolves allowlisted browser requests into a
// config.SessionModelPolicy without ever exposing raw provider/model or
// description values.
type LaunchConfiguration struct {
	modelDefs       map[string]config.ModelDefinition
	modelOrder      []string
	workerDefs      map[string]config.WorkerDefaults
	workerOrder     []string
	defaultRoot     ModelSelection
	repositories    []RepositoryLaunchOption
	workspaceBySlug map[string]config.WorkspaceStart
}

// NewLaunchConfiguration derives the immutable launch catalog from a validated
// config. It resolves and validates the default root and every worker default
// so a bad default fails at construction rather than at request time.
func NewLaunchConfiguration(cfg config.Config) (LaunchConfiguration, error) {
	if len(cfg.Models) == 0 {
		return LaunchConfiguration{}, fmt.Errorf("at least one model is required")
	}

	lc := LaunchConfiguration{
		modelDefs:       make(map[string]config.ModelDefinition, len(cfg.Models)),
		modelOrder:      make([]string, 0, len(cfg.Models)),
		workerDefs:      make(map[string]config.WorkerDefaults, len(cfg.Workers)),
		workspaceBySlug: make(map[string]config.WorkspaceStart),
	}
	for name, def := range cfg.Models {
		def.ThinkingLevels = append([]string(nil), def.ThinkingLevels...)
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

	// Derive the immutable repository launch catalog from configured
	// workspace.repos. Invalid configured repositories fail construction rather
	// than at request time. The parsed result is slug-sorted and immutable.
	repos, err := config.ParseWorkspaceRepositories(cfg.Workspace.Repos)
	if err != nil {
		return LaunchConfiguration{}, fmt.Errorf("resolve workspace repositories: %w", err)
	}
	lc.repositories = make([]RepositoryLaunchOption, 0, len(repos))
	for _, repo := range repos {
		lc.repositories = append(lc.repositories, RepositoryLaunchOption{Slug: repo.Slug})
		lc.workspaceBySlug[repo.Slug] = config.WorkspaceStart{Repository: repo.Slug, Checkout: repo.Checkout}
	}
	return lc, nil
}

// Resolve validates an allowlisted request against the fixed catalog and worker
// role set, then returns an immutable resolved launch state. The name and
// repository are normalized/validated first; the model policy is assembled
// after, and every input failure is a typed invalid request.
func (lc LaunchConfiguration) Resolve(request SessionLaunchRequest) (ResolvedSessionLaunch, error) {
	name, err := lc.resolveName(request.Name)
	if err != nil {
		return ResolvedSessionLaunch{}, contractErr(err)
	}
	workspace, err := lc.resolveWorkspace(request.Repository)
	if err != nil {
		return ResolvedSessionLaunch{}, contractErr(err)
	}
	root, err := lc.resolveModelProfile(request.Root.ModelType, request.Root.ThinkingLevel)
	if err != nil {
		return ResolvedSessionLaunch{}, contractErr(err)
	}
	if len(request.Workers) != len(lc.workerDefs) {
		return ResolvedSessionLaunch{}, contractErr(fmt.Errorf("launch request must include every worker exactly once"))
	}

	policy := config.SessionModelPolicy{
		Root:    root,
		Workers: make(map[string]config.WorkerProfile, len(request.Workers)),
	}
	seen := make(map[string]struct{}, len(request.Workers))
	for _, sel := range request.Workers {
		if sel.WorkerType == "" {
			return ResolvedSessionLaunch{}, contractErr(fmt.Errorf("launch request worker type is required"))
		}
		if _, dup := seen[sel.WorkerType]; dup {
			return ResolvedSessionLaunch{}, contractErr(fmt.Errorf("launch request includes duplicate worker %q", sel.WorkerType))
		}
		seen[sel.WorkerType] = struct{}{}
		def, ok := lc.workerDefs[sel.WorkerType]
		if !ok {
			return ResolvedSessionLaunch{}, contractErr(fmt.Errorf("launch request includes unknown worker %q", sel.WorkerType))
		}
		profile, err := lc.resolveModelProfile(sel.ModelType, sel.ThinkingLevel)
		if err != nil {
			return ResolvedSessionLaunch{}, contractErr(err)
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
			return ResolvedSessionLaunch{}, contractErr(fmt.Errorf("launch request is missing worker %q", name))
		}
	}
	if err := policy.Validate(); err != nil {
		return ResolvedSessionLaunch{}, contractErr(err)
	}
	return ResolvedSessionLaunch{Name: name, Workspace: workspace, Policy: policy.Clone()}, nil
}

// resolveName trims surrounding whitespace and validates the optional display
// name: at most 80 Unicode code points and no control characters. An empty (or
// whitespace-only) name resolves to the empty default.
func (lc LaunchConfiguration) resolveName(name string) (string, error) {
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("session name contains a control character")
		}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if utf8.RuneCountInString(name) > 80 {
		return "", fmt.Errorf("session name must be at most 80 characters")
	}
	return name, nil
}

// resolveWorkspace resolves an optional repository slug into the immutable
// configured workspace start. An empty selection is the /workspace default;
// any other value must exactly match a configured slug.
func (lc LaunchConfiguration) resolveWorkspace(repository string) (config.WorkspaceStart, error) {
	if repository == "" {
		return config.WorkspaceStart{}, nil
	}
	start, ok := lc.workspaceBySlug[repository]
	if !ok {
		return config.WorkspaceStart{}, fmt.Errorf("unknown repository %q", repository)
	}
	return start, nil
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
	repositories := make([]RepositoryLaunchOption, len(lc.repositories))
	copy(repositories, lc.repositories)
	return SessionLaunchOptions{Models: models, Root: lc.defaultRoot, Workers: workers, Repositories: repositories}
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
