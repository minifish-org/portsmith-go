// Resource discovery and assembly for the embedded SDK.
//
// This file ports the SDK-facing half of
// packages/coding-agent/src/core/resource-loader.ts, source-info.ts,
// project-trust.ts and trust-manager.ts from Pi at revision
// f07218c4d4bbc12bef056a7058c3dd49dfe41abe.
//
// The upstream loader resolves package-manager resources, npm/Git extension
// packages, themes, interactive trust prompts and remote services. Those are
// outside this increment. What remains, and what this file implements, is the
// headless resource contract: explicit Cwd/AgentDir plus caller-supplied paths,
// project AGENTS.md discovery, plain-text skills and prompt templates, a
// structured system prompt, and an explicit on-disk project trust store.
//
// Discovery never reads the user's home directory unless the caller passes it
// as AgentDir, never runs code, never installs packages and never makes network
// requests. Repeated LoadResources calls re-read the filesystem, so edits are
// reflected without any process-wide cache.
//
// Upstream: Copyright (c) 2025 Mario Zechner, MIT License.
package codingagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ContextFile is a project instruction file (AGENTS.md family) with its
// resolved path and decoded content.
type ContextFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// SourceScope is where a resource came from.
type SourceScope string

// Source scopes.
const (
	SourceScopeUser      SourceScope = "user"
	SourceScopeProject   SourceScope = "project"
	SourceScopeTemporary SourceScope = "temporary"
)

// SourceOrigin distinguishes top-level resources from package-provided ones.
type SourceOrigin string

// Source origins.
const (
	SourceOriginPackage  SourceOrigin = "package"
	SourceOriginTopLevel SourceOrigin = "top-level"
)

// SourceInfo records where a loaded resource came from.
type SourceInfo struct {
	Path    string       `json:"path"`
	Source  string       `json:"source"`
	Scope   SourceScope  `json:"scope"`
	Origin  SourceOrigin `json:"origin"`
	BaseDir string       `json:"baseDir,omitempty"`
}

// PathMetadata is the provenance attached to an explicitly extended path.
type PathMetadata struct {
	Source  string       `json:"source"`
	Scope   SourceScope  `json:"scope"`
	Origin  SourceOrigin `json:"origin"`
	BaseDir string       `json:"baseDir,omitempty"`
}

// SyntheticSourceInfoOptions configures CreateSyntheticSourceInfo.
type SyntheticSourceInfoOptions struct {
	Source  string
	Scope   SourceScope
	Origin  SourceOrigin
	BaseDir string
}

// CreateSourceInfo builds a SourceInfo from caller-supplied path metadata.
func CreateSourceInfo(path string, metadata PathMetadata) SourceInfo {
	return SourceInfo{
		Path:    path,
		Source:  metadata.Source,
		Scope:   metadata.Scope,
		Origin:  metadata.Origin,
		BaseDir: metadata.BaseDir,
	}
}

// CreateSyntheticSourceInfo builds a SourceInfo for resources discovered
// without package metadata. Scope defaults to temporary and origin to
// top-level.
func CreateSyntheticSourceInfo(path string, options SyntheticSourceInfoOptions) SourceInfo {
	scope := options.Scope
	if scope == "" {
		scope = SourceScopeTemporary
	}
	origin := options.Origin
	if origin == "" {
		origin = SourceOriginTopLevel
	}
	return SourceInfo{
		Path:    path,
		Source:  options.Source,
		Scope:   scope,
		Origin:  origin,
		BaseDir: options.BaseDir,
	}
}

// ResourceCollision describes two resources that resolved to the same name.
type ResourceCollision struct {
	ResourceType string `json:"resourceType"`
	Name         string `json:"name"`
	WinnerPath   string `json:"winnerPath"`
	LoserPath    string `json:"loserPath"`
}

// ResourceDiagnostic is a warning/collision emitted while loading resources.
type ResourceDiagnostic struct {
	Type      string             `json:"type"`
	Message   string             `json:"message"`
	Path      string             `json:"path,omitempty"`
	Code      string             `json:"code,omitempty"`
	Collision *ResourceCollision `json:"collision,omitempty"`
}

// DiagnosticString renders a diagnostic as a single caller-facing line.
func DiagnosticString(diagnostic ResourceDiagnostic) string {
	if diagnostic.Path != "" {
		return diagnostic.Path + ": " + diagnostic.Message
	}
	return diagnostic.Message
}

// ResourcePathEntry is a path plus its provenance for extension resources.
type ResourcePathEntry struct {
	Path     string
	Metadata PathMetadata
}

// ResourceExtensionPaths are additional skill/template paths supplied after a
// loader has already been created. Themes are intentionally not modelled.
type ResourceExtensionPaths struct {
	SkillPaths  []ResourcePathEntry
	PromptPaths []ResourcePathEntry
}

// ProjectTrustContext describes the host context for a trust decision.
type ProjectTrustContext struct {
	Cwd   string
	HasUI bool
}

// ResourceLoaderReloadOptions configures a reload. ResolveProjectTrust, when
// set, receives the host trust context and returns the effective decision.
type ResourceLoaderReloadOptions struct {
	ResolveProjectTrust func(ProjectTrustContext) (bool, error)
}

// ResourceLoader is the headless resource loader surface. Extension/package
// management and themes are intentionally excluded.
type ResourceLoader interface {
	GetSkills() LoadSkillsResult
	GetPrompts() LoadPromptTemplatesResult
	GetAgentsFiles() []ContextFile
	GetSystemPrompt() string
	GetSystemPromptSource() string
	GetAppendSystemPrompt() []string
	GetAppendSystemPromptSources() []string
	ExtendResources(paths ResourceExtensionPaths)
	Reload(options *ResourceLoaderReloadOptions) error
}

// contextFileCandidates is the precedence-ordered set of project instruction
// file names checked in each directory.
var contextFileCandidates = []string{"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

func loadContextFileFromDir(dir string) (ContextFile, bool) {
	if dir == "" {
		return ContextFile{}, false
	}
	for _, name := range contextFileCandidates {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return ContextFile{Path: path, Content: string(stripBOM(content))}, true
	}
	return ContextFile{}, false
}

// LoadProjectContextFiles discovers AGENTS.md-style instruction files from the
// global agent directory and the ancestors of cwd. Global content comes first,
// then ancestors from the outermost directory down to cwd. Duplicate paths are
// skipped.
func LoadProjectContextFiles(cwd, agentDir string) []ContextFile {
	result := []ContextFile{}
	seen := map[string]bool{}

	if agentDir != "" {
		if file, ok := loadContextFileFromDir(resolveExistingPath(agentDir)); ok {
			result = append(result, file)
			seen[file.Path] = true
		}
	}

	ancestors := []ContextFile{}
	if cwd != "" {
		current, err := filepath.Abs(cwd)
		if err != nil {
			current = cwd
		}
		for {
			if file, ok := loadContextFileFromDir(current); ok && !seen[file.Path] {
				ancestors = append([]ContextFile{file}, ancestors...)
				seen[file.Path] = true
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
	}
	result = append(result, ancestors...)
	return result
}

func resolveExistingPath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(expandTilde(path))
	if err != nil {
		return path
	}
	return absolute
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// resolvePromptInput reads a prompt source when it names a file, otherwise it
// returns the literal text. An empty file yields an explicit empty prompt.
func resolvePromptInput(input string) string {
	if input == "" {
		return ""
	}
	if fileExists(input) {
		content, err := os.ReadFile(input)
		if err == nil {
			return string(stripBOM(content))
		}
	}
	return input
}

func discoverSystemPromptFile(cwd, agentDir string) string {
	if cwd != "" {
		path := filepath.Join(cwd, ConfigDirName, "SYSTEM.md")
		if fileExists(path) {
			return path
		}
	}
	if agentDir != "" {
		path := filepath.Join(agentDir, "SYSTEM.md")
		if fileExists(path) {
			return path
		}
	}
	return ""
}

func discoverAppendSystemPromptFile(cwd, agentDir string) string {
	if cwd != "" {
		path := filepath.Join(cwd, ConfigDirName, "APPEND_SYSTEM.md")
		if fileExists(path) {
			return path
		}
	}
	if agentDir != "" {
		path := filepath.Join(agentDir, "APPEND_SYSTEM.md")
		if fileExists(path) {
			return path
		}
	}
	return ""
}

// ResourceOptions are the explicit inputs to LoadResources. No implicit home
// directory discovery happens: only supplied paths are considered.
type ResourceOptions struct {
	Cwd                string
	AgentDir           string
	SkillPaths         []string
	TemplatePaths      []string
	ContextFiles       []string
	SystemPrompt       string
	AppendSystemPrompt []string
}

// ResourceSet is the assembled, headless resource view.
type ResourceSet struct {
	SystemPrompt string
	Skills       []Skill
	Templates    []PromptTemplate
	ContextFiles []string
	Diagnostics  []string
}

type resourceBundleOptions struct {
	cwd                  string
	agentDir             string
	skillPaths           []string
	templatePaths        []string
	contextFiles         []string
	systemPrompt         string
	appendSystemPrompt   []string
	explicitSystemPrompt bool
}

type loadedResourceBundle struct {
	skills             LoadSkillsResult
	prompts            LoadPromptTemplatesResult
	contextFiles       []ContextFile
	systemPrompt       string
	appendSystemPrompt []string
	systemPromptSource string
	appendSources      []string
	diagnostics        []ResourceDiagnostic
}

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// loadResourceBundle performs the whole discovery pass. It is deliberately
// stateless: every call re-reads the filesystem.
func loadResourceBundle(options resourceBundleOptions) (loadedResourceBundle, error) {
	bundle := loadedResourceBundle{
		skills:             LoadSkillsResult{Skills: []Skill{}, Diagnostics: []ResourceDiagnostic{}},
		prompts:            LoadPromptTemplatesResult{Templates: []PromptTemplate{}, Diagnostics: []ResourceDiagnostic{}},
		appendSystemPrompt: []string{},
	}

	cwd := options.cwd
	agentDir := options.agentDir

	// Context files: explicit caller files first, then discovered AGENTS.md.
	contextFiles := []ContextFile{}
	seenPaths := map[string]bool{}
	for _, path := range options.contextFiles {
		resolved := resolveExistingPath(path)
		if !fileExists(resolved) {
			bundle.diagnostics = append(bundle.diagnostics, ResourceDiagnostic{
				Type:    "warning",
				Message: "context file does not exist",
				Path:    resolved,
			})
			continue
		}
		content, err := os.ReadFile(resolved)
		if err != nil {
			bundle.diagnostics = append(bundle.diagnostics, ResourceDiagnostic{
				Type:    "warning",
				Message: err.Error(),
				Path:    resolved,
			})
			continue
		}
		contextFiles = append(contextFiles, ContextFile{Path: resolved, Content: string(stripBOM(content))})
		seenPaths[resolved] = true
	}
	for _, file := range LoadProjectContextFiles(cwd, agentDir) {
		if seenPaths[file.Path] {
			continue
		}
		seenPaths[file.Path] = true
		contextFiles = append(contextFiles, file)
	}
	bundle.contextFiles = contextFiles

	// Skills: explicit paths win, then project, then user.
	skillPaths := append([]string(nil), options.skillPaths...)
	if cwd != "" {
		if projectSkills := filepath.Join(cwd, ConfigDirName, "skills"); dirExists(projectSkills) {
			skillPaths = append(skillPaths, projectSkills)
		}
	}
	if agentDir != "" {
		if userSkills := filepath.Join(agentDir, "skills"); dirExists(userSkills) {
			skillPaths = append(skillPaths, userSkills)
		}
	}
	bundle.skills = LoadSkills(LoadSkillsOptions{Cwd: cwd, AgentDir: agentDir, SkillPaths: skillPaths, IncludeDefaults: false})

	// Prompt templates: same precedence ordering.
	templatePaths := append([]string(nil), options.templatePaths...)
	if cwd != "" {
		if projectPrompts := filepath.Join(cwd, ConfigDirName, "prompts"); dirExists(projectPrompts) {
			templatePaths = append(templatePaths, projectPrompts)
		}
	}
	if agentDir != "" {
		if userPrompts := filepath.Join(agentDir, "prompts"); dirExists(userPrompts) {
			templatePaths = append(templatePaths, userPrompts)
		}
	}
	bundle.prompts = LoadPromptTemplates(LoadPromptTemplatesOptions{Cwd: cwd, AgentDir: agentDir, PromptPaths: templatePaths, IncludeDefaults: false})

	// System prompt and append prompts.
	systemPromptSource := options.systemPrompt
	if systemPromptSource == "" {
		systemPromptSource = discoverSystemPromptFile(cwd, agentDir)
	}
	if systemPromptSource != "" {
		bundle.systemPrompt = resolvePromptInput(systemPromptSource)
		bundle.systemPromptSource = systemPromptSource
	}

	appendSources := append([]string(nil), options.appendSystemPrompt...)
	if len(appendSources) == 0 {
		if discovered := discoverAppendSystemPromptFile(cwd, agentDir); discovered != "" {
			appendSources = []string{discovered}
		}
	}
	for _, source := range appendSources {
		resolved := resolvePromptInput(source)
		bundle.appendSystemPrompt = append(bundle.appendSystemPrompt, resolved)
		if fileExists(source) {
			bundle.appendSources = append(bundle.appendSources, resolveExistingPath(source))
		}
	}

	return bundle, nil
}

func buildResourceSystemPrompt(bundle loadedResourceBundle, options resourceBundleOptions) (string, error) {
	return BuildSystemPrompt(BuildSystemPromptOptions{
		CustomPrompt:         bundle.systemPrompt,
		ExplicitSystemPrompt: options.explicitSystemPrompt && bundle.systemPromptSource != "",
		AppendSystemPrompt:   bundle.appendSystemPrompt,
		ContextFiles:         bundle.contextFiles,
		Skills:               bundle.skills.Skills,
		Cwd:                  options.cwd,
	})
}

// LoadResources discovers and assembles resources from the explicit options.
// It reports missing explicit paths and duplicate names through Diagnostics
// rather than failing, so callers can surface partial results.
func LoadResources(options ResourceOptions) (ResourceSet, error) {
	bundleOptions := resourceBundleOptions{
		cwd:                  resolveExistingPath(options.Cwd),
		agentDir:             resolveExistingPath(options.AgentDir),
		skillPaths:           options.SkillPaths,
		templatePaths:        options.TemplatePaths,
		contextFiles:         options.ContextFiles,
		systemPrompt:         options.SystemPrompt,
		appendSystemPrompt:   options.AppendSystemPrompt,
		explicitSystemPrompt: options.SystemPrompt != "",
	}
	bundle, err := loadResourceBundle(bundleOptions)
	if err != nil {
		return ResourceSet{}, err
	}

	systemPrompt, err := buildResourceSystemPrompt(bundle, bundleOptions)
	if err != nil {
		return ResourceSet{}, err
	}

	diagnostics := []ResourceDiagnostic{}
	diagnostics = append(diagnostics, bundle.skills.Diagnostics...)
	diagnostics = append(diagnostics, bundle.prompts.Diagnostics...)
	diagnostics = append(diagnostics, bundle.diagnostics...)

	contextPaths := make([]string, 0, len(bundle.contextFiles))
	for _, file := range bundle.contextFiles {
		contextPaths = append(contextPaths, file.Path)
	}

	diagnosticStrings := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		diagnosticStrings = append(diagnosticStrings, DiagnosticString(diagnostic))
	}

	return ResourceSet{
		SystemPrompt: systemPrompt,
		Skills:       bundle.skills.Skills,
		Templates:    bundle.prompts.Templates,
		ContextFiles: contextPaths,
		Diagnostics:  diagnosticStrings,
	}, nil
}

// DefaultResourceLoaderOptions configures a DefaultResourceLoader.
type DefaultResourceLoaderOptions struct {
	Cwd                           string
	AgentDir                      string
	Settings                      *SettingsManager
	NoSkills                      bool
	NoPromptTemplates             bool
	NoContextFiles                bool
	ContextFiles                  []string
	AdditionalSkillPaths          []string
	AdditionalPromptTemplatePaths []string
	SystemPrompt                  string
	AppendSystemPrompt            []string
	SkillsOverride                func(LoadSkillsResult) LoadSkillsResult
	PromptsOverride               func(LoadPromptTemplatesResult) LoadPromptTemplatesResult
	AgentsFilesOverride           func([]ContextFile) []ContextFile
	SystemPromptOverride          func(string) string
	AppendSystemPromptOverride    func([]string) []string
}

// DefaultResourceLoader is the stateful loader used by the agent session. It
// re-reads the filesystem on every Reload and holds no process-wide state.
type DefaultResourceLoader struct {
	options        DefaultResourceLoaderOptions
	projectTrusted bool

	skills             LoadSkillsResult
	prompts            LoadPromptTemplatesResult
	contextFiles       []ContextFile
	systemPrompt       string
	systemPromptSource string
	appendSystemPrompt []string
	appendSources      []string
	diagnostics        []ResourceDiagnostic
	loaded             bool
}

// NewDefaultResourceLoader creates a loader for the supplied options.
func NewDefaultResourceLoader(options DefaultResourceLoaderOptions) *DefaultResourceLoader {
	return &DefaultResourceLoader{options: options}
}

func (l *DefaultResourceLoader) reload() {
	options := l.options
	bundleOptions := resourceBundleOptions{
		cwd:                  resolveExistingPath(options.Cwd),
		agentDir:             resolveExistingPath(options.AgentDir),
		contextFiles:         options.ContextFiles,
		systemPrompt:         options.SystemPrompt,
		appendSystemPrompt:   options.AppendSystemPrompt,
		explicitSystemPrompt: options.SystemPrompt != "",
	}
	if !options.NoSkills {
		bundleOptions.skillPaths = options.AdditionalSkillPaths
	}
	if !options.NoPromptTemplates {
		bundleOptions.templatePaths = options.AdditionalPromptTemplatePaths
	}

	bundle, err := loadResourceBundle(bundleOptions)
	if err != nil {
		l.diagnostics = []ResourceDiagnostic{{Type: "error", Message: err.Error()}}
		return
	}

	skills := bundle.skills
	if options.SkillsOverride != nil {
		skills = options.SkillsOverride(skills)
	}
	prompts := bundle.prompts
	if options.PromptsOverride != nil {
		prompts = options.PromptsOverride(prompts)
	}
	contextFiles := bundle.contextFiles
	if options.NoContextFiles {
		contextFiles = []ContextFile{}
	}
	if options.AgentsFilesOverride != nil {
		contextFiles = options.AgentsFilesOverride(contextFiles)
	}

	systemPrompt := bundle.systemPrompt
	if options.SystemPromptOverride != nil {
		systemPrompt = options.SystemPromptOverride(systemPrompt)
	}
	appendSystemPrompt := bundle.appendSystemPrompt
	if options.AppendSystemPromptOverride != nil {
		appendSystemPrompt = options.AppendSystemPromptOverride(appendSystemPrompt)
	}

	l.skills = skills
	l.prompts = prompts
	l.contextFiles = contextFiles
	l.systemPrompt = systemPrompt
	l.systemPromptSource = bundle.systemPromptSource
	l.appendSystemPrompt = appendSystemPrompt
	l.appendSources = bundle.appendSources
	l.diagnostics = append(append([]ResourceDiagnostic{}, bundle.skills.Diagnostics...), bundle.prompts.Diagnostics...)
	l.diagnostics = append(l.diagnostics, bundle.diagnostics...)
	l.loaded = true
}

// GetSkills returns the current skills and diagnostics.
func (l *DefaultResourceLoader) GetSkills() LoadSkillsResult {
	if !l.loaded {
		l.reload()
	}
	return l.skills
}

// GetPrompts returns the current prompt templates and diagnostics.
func (l *DefaultResourceLoader) GetPrompts() LoadPromptTemplatesResult {
	if !l.loaded {
		l.reload()
	}
	return l.prompts
}

// GetAgentsFiles returns the discovered project instruction files.
func (l *DefaultResourceLoader) GetAgentsFiles() []ContextFile {
	if !l.loaded {
		l.reload()
	}
	return l.contextFiles
}

// GetSystemPrompt returns the resolved custom system prompt, if any.
func (l *DefaultResourceLoader) GetSystemPrompt() string {
	if !l.loaded {
		l.reload()
	}
	return l.systemPrompt
}

// GetSystemPromptSource returns the file the system prompt was read from.
func (l *DefaultResourceLoader) GetSystemPromptSource() string {
	return l.systemPromptSource
}

// GetAppendSystemPrompt returns the resolved append prompts.
func (l *DefaultResourceLoader) GetAppendSystemPrompt() []string {
	if !l.loaded {
		l.reload()
	}
	return l.appendSystemPrompt
}

// GetAppendSystemPromptSources returns the append prompt files, if any.
func (l *DefaultResourceLoader) GetAppendSystemPromptSources() []string {
	return l.appendSources
}

// GetDiagnostics returns the diagnostics collected during the last reload.
func (l *DefaultResourceLoader) GetDiagnostics() []ResourceDiagnostic {
	if !l.loaded {
		l.reload()
	}
	return l.diagnostics
}

// ExtendResources appends additional skill/template paths and reloads.
func (l *DefaultResourceLoader) ExtendResources(paths ResourceExtensionPaths) {
	for _, entry := range paths.SkillPaths {
		l.options.AdditionalSkillPaths = append(l.options.AdditionalSkillPaths, entry.Path)
	}
	for _, entry := range paths.PromptPaths {
		l.options.AdditionalPromptTemplatePaths = append(l.options.AdditionalPromptTemplatePaths, entry.Path)
	}
	l.loaded = false
	l.reload()
}

// Reload re-runs discovery, optionally resolving project trust first.
func (l *DefaultResourceLoader) Reload(options *ResourceLoaderReloadOptions) error {
	if options != nil && options.ResolveProjectTrust != nil {
		trusted, err := options.ResolveProjectTrust(ProjectTrustContext{Cwd: l.options.Cwd})
		if err != nil {
			return err
		}
		l.projectTrusted = trusted
	}
	l.loaded = false
	l.reload()
	return nil
}

// ---------------------------------------------------------------------------
// Project trust

// AppMode is the host application mode. Interactive trust prompts are excluded
// from the headless SDK; the type remains for parity with the source.
type AppMode string

// Application modes.
const (
	AppModeInteractive AppMode = "interactive"
	AppModePrint       AppMode = "print"
	AppModeJson        AppMode = "json"
	AppModeRPC         AppMode = "rpc"
)

// ProjectTrustDecision is a stored trust decision: true, false or nil (unset).
type ProjectTrustDecision *bool

// ProjectTrustStoreEntry is a decision plus the directory it applies to.
type ProjectTrustStoreEntry struct {
	Path     string
	Decision ProjectTrustDecision
}

// ProjectTrustUpdate is a pending trust store mutation. A nil decision deletes
// the entry.
type ProjectTrustUpdate struct {
	Path     string
	Decision ProjectTrustDecision
}

// ProjectTrustOption is one selectable trust option.
type ProjectTrustOption struct {
	Label     string
	Trusted   bool
	Updates   []ProjectTrustUpdate
	SavedPath string
}

// ResolveProjectTrustedOptions configures ResolveProjectTrusted.
type ResolveProjectTrustedOptions struct {
	Cwd                 string
	TrustStore          *ProjectTrustStore
	TrustOverride       *bool
	DefaultProjectTrust DefaultProjectTrust
	HasUI               bool
}

// GetProjectTrustParentPath returns the parent directory used by the
// "trust parent folder" option.
func GetProjectTrustParentPath(cwd string) (string, bool) {
	normalized := normalizeTrustPath(cwd)
	parent := filepath.Dir(normalized)
	if parent == normalized {
		return "", false
	}
	return parent, true
}

// GetProjectTrustOptions returns the trust choices for cwd.
func GetProjectTrustOptions(cwd string, includeSessionOnly bool) []ProjectTrustOption {
	trustPath := normalizeTrustPath(cwd)
	options := []ProjectTrustOption{
		{
			Label:     "Trust",
			Trusted:   true,
			Updates:   []ProjectTrustUpdate{{Path: trustPath, Decision: boolPointer(true)}},
			SavedPath: trustPath,
		},
	}
	if parent, ok := GetProjectTrustParentPath(cwd); ok {
		options = append(options, ProjectTrustOption{
			Label:   "Trust parent folder (" + parent + ")",
			Trusted: true,
			Updates: []ProjectTrustUpdate{
				{Path: parent, Decision: boolPointer(true)},
				{Path: trustPath, Decision: nil},
			},
			SavedPath: parent,
		})
	}
	if includeSessionOnly {
		options = append(options, ProjectTrustOption{Label: "Trust (this session only)", Trusted: true})
	}
	options = append(options, ProjectTrustOption{
		Label:     "Do not trust",
		Trusted:   false,
		Updates:   []ProjectTrustUpdate{{Path: trustPath, Decision: boolPointer(false)}},
		SavedPath: trustPath,
	})
	if includeSessionOnly {
		options = append(options, ProjectTrustOption{Label: "Do not trust (this session only)", Trusted: false})
	}
	return options
}

var trustRequiringProjectEntries = []string{
	"settings.json",
	"extensions",
	"skills",
	"prompts",
	"themes",
	"SYSTEM.md",
	"APPEND_SYSTEM.md",
}

// HasTrustRequiringProjectResources reports whether cwd has project-local
// resources that must be gated by project trust.
func HasTrustRequiringProjectResources(cwd string) bool {
	current := normalizeTrustPath(cwd)
	configDir := filepath.Join(current, ConfigDirName)
	for _, entry := range trustRequiringProjectEntries {
		if _, err := os.Stat(filepath.Join(configDir, entry)); err == nil {
			return true
		}
	}

	home := ""
	if value, err := os.UserHomeDir(); err == nil {
		home = normalizeTrustPath(value)
	}
	userAgentsSkills := filepath.Join(home, ".agents", "skills")

	for {
		agentsSkills := filepath.Join(current, ".agents", "skills")
		if agentsSkills != userAgentsSkills {
			if info, err := os.Stat(agentsSkills); err == nil && info.IsDir() {
				return true
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

func normalizeTrustPath(cwd string) string {
	if cwd == "" {
		return "/"
	}
	absolute, err := filepath.Abs(expandTilde(cwd))
	if err != nil {
		absolute = cwd
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return filepath.Clean(absolute)
}

// ProjectTrustStore is a JSON-backed trust store at agentDir/trust.json.
type ProjectTrustStore struct {
	trustPath string
	mu        sync.Mutex
}

// NewProjectTrustStore returns a store rooted at the supplied agent directory.
func NewProjectTrustStore(agentDir string) *ProjectTrustStore {
	return &ProjectTrustStore{trustPath: filepath.Join(resolveExistingPath(agentDir), "trust.json")}
}

// Get returns the nearest stored decision, or nil.
func (s *ProjectTrustStore) Get(cwd string) ProjectTrustDecision {
	entry := s.GetEntry(cwd)
	if entry == nil {
		return nil
	}
	return entry.Decision
}

// GetEntry returns the nearest stored entry, or nil.
func (s *ProjectTrustStore) GetEntry(cwd string) *ProjectTrustStoreEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.readFileLocked()
	if err != nil {
		return nil
	}
	return findNearestTrustEntry(data, cwd)
}

// Set writes a decision for cwd.
func (s *ProjectTrustStore) Set(cwd string, decision ProjectTrustDecision) error {
	return s.SetMany([]ProjectTrustUpdate{{Path: cwd, Decision: decision}})
}

// SetMany applies a batch of trust updates.
func (s *ProjectTrustStore) SetMany(updates []ProjectTrustUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.readFileLocked()
	if err != nil {
		return err
	}
	for _, update := range updates {
		key := normalizeTrustPath(update.Path)
		if update.Decision == nil {
			delete(data, key)
			continue
		}
		data[key] = update.Decision
	}
	return s.writeFileLocked(data)
}

func (s *ProjectTrustStore) readFileLocked() (map[string]ProjectTrustDecision, error) {
	data := map[string]ProjectTrustDecision{}
	raw, err := os.ReadFile(s.trustPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return data, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return data, nil
	}
	var parsed map[string]*bool
	if err := json.Unmarshal(stripBOM(raw), &parsed); err != nil {
		return nil, fmt.Errorf("failed to read trust store %s: %w", s.trustPath, err)
	}
	for key, value := range parsed {
		data[key] = ProjectTrustDecision(value)
	}
	return data, nil
}

func (s *ProjectTrustStore) writeFileLocked(data map[string]ProjectTrustDecision) error {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	serializable := map[string]*bool{}
	for _, key := range keys {
		value := data[key]
		if value == nil {
			continue
		}
		serializable[key] = (*bool)(value)
	}
	encoded, err := json.MarshalIndent(serializable, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(s.trustPath, encoded, 0o600)
}

func findNearestTrustEntry(data map[string]ProjectTrustDecision, cwd string) *ProjectTrustStoreEntry {
	current := normalizeTrustPath(cwd)
	for {
		if value, ok := data[current]; ok && value != nil {
			return &ProjectTrustStoreEntry{Path: current, Decision: value}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// ResolveProjectTrusted applies an override, the persisted store, then the
// configured default policy. Without a UI the ask policy resolves to false.
func ResolveProjectTrusted(options ResolveProjectTrustedOptions) (bool, error) {
	if options.TrustOverride != nil {
		return *options.TrustOverride, nil
	}
	if !HasTrustRequiringProjectResources(options.Cwd) {
		return true, nil
	}
	if options.TrustStore != nil {
		if decision := options.TrustStore.Get(options.Cwd); decision != nil {
			return *decision, nil
		}
	}
	switch options.DefaultProjectTrust {
	case ProjectTrustAlways:
		return true, nil
	case ProjectTrustNever:
		return false, nil
	}
	if !options.HasUI {
		return false, nil
	}
	// The interactive selection UI is outside this increment; a host that
	// reports HasUI must resolve trust itself via TrustOverride.
	return false, nil
}
