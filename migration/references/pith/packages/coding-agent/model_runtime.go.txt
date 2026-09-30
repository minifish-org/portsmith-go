// Model runtime, model resolution and provider composition for the embedded
// SDK. This file ports the model-runtime/config half of
// packages/coding-agent/src/core from Pi at revision
// f07218c4d4bbc12bef056a7058c3dd49dfe41abe and reuses the accepted Pith AI
// catalog, auth and provider abstractions for real requests.
//
// Upstream: Copyright (c) 2025 Mario Zechner, MIT License.
package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	"github.com/minifish-org/pith/packages/ai"
	"github.com/minifish-org/pith/packages/ai/api"
	"github.com/minifish-org/pith/packages/ai/auth"
	authtypes "github.com/minifish-org/pith/packages/ai/auth/types"
	"github.com/minifish-org/pith/packages/ai/providers"
	aitypes "github.com/minifish-org/pith/packages/ai/types"
)

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

// DefaultThinkingLevel is the thinking level used when none is configured.
const DefaultThinkingLevel agenttypes.ThinkingLevel = agenttypes.ThinkingMedium

// ThinkingLevelOptions lists the supported thinking levels in declaration
// order.
var ThinkingLevelOptions = []agenttypes.ThinkingLevel{
	agenttypes.ThinkingOff,
	agenttypes.ThinkingMinimal,
	agenttypes.ThinkingLow,
	agenttypes.ThinkingMedium,
	agenttypes.ThinkingHigh,
	agenttypes.ThinkingXHigh,
	agenttypes.ThinkingMax,
}

func isValidThinkingLevel(value string) bool {
	for _, level := range ThinkingLevelOptions {
		if string(level) == value {
			return true
		}
	}
	return false
}

// DefaultModelPerProvider maps each provider to its preferred default model.
var DefaultModelPerProvider = map[string]string{
	"amazon-bedrock":             "us.anthropic.claude-opus-4-6-v1",
	"ant-ling":                   "Ring-2.6-1T",
	"anthropic":                  "claude-opus-4-8",
	"openai":                     "gpt-5.5",
	"azure-openai-responses":     "gpt-5.4",
	"openai-codex":               "gpt-5.5",
	"radius":                     "balanced",
	"nvidia":                     "nvidia/nemotron-3-super-120b-a12b",
	"deepseek":                   "deepseek-v4-pro",
	"google":                     "gemini-3.1-pro-preview",
	"google-vertex":              "gemini-3.1-pro-preview",
	"github-copilot":             "gpt-5.4",
	"openrouter":                 "moonshotai/kimi-k2.6",
	"vercel-ai-gateway":          "zai/glm-5.1",
	"xai":                        "grok-4.7",
	"groq":                       "openai/gpt-oss-120b",
	"cerebras":                   "gpt-oss-120b",
	"zai":                        "glm-5.3",
	"zai-coding-cn":              "glm-5.3",
	"mistral":                    "devstral-medium-latest",
	"minimax":                    "MiniMax-M2.7",
	"minimax-cn":                 "MiniMax-M2.7",
	"moonshotai":                 "kimi-k2.6",
	"moonshotai-cn":              "kimi-k2.6",
	"huggingface":                "moonshotai/Kimi-K2.6",
	"fireworks":                  "accounts/fireworks/models/kimi-k2p6",
	"together":                   "moonshotai/Kimi-K2.6",
	"baseten":                    "zai-org/GLM-5.2",
	"opencode":                   "kimi-k2.6",
	"opencode-go":                "kimi-k2.6",
	"kimi-coding":                "kimi-for-coding",
	"meta":                       "muse-spark-1.3",
	"cloudflare-workers-ai":      "@cf/moonshotai/kimi-k2.6",
	"cloudflare-ai-gateway":      "workers-ai/@cf/moonshotai/kimi-k2.6",
	"qwen-token-plan":            "qwen3.7-max",
	"qwen-token-plan-cn":         "qwen3.7-max",
	"qwen-token-plan-individual": "qwen3.8-max",
	"xiaomi":                     "mimo-v2.5-pro",
	"xiaomi-token-plan-cn":       "mimo-v2.5-pro",
	"xiaomi-token-plan-ams":      "mimo-v2.5-pro",
	"xiaomi-token-plan-sgp":      "mimo-v2.5-pro",
}

// ---------------------------------------------------------------------------
// Model options and resolution
// ---------------------------------------------------------------------------

// ModelOptions selects a model and the caller-owned capabilities that travel
// with it. MaxTokens/ContextWindow are explicit overrides; nil means "use the
// model's own capacity". APIKey and StreamFn are injected per session so the
// SDK never owns ambient credentials or a global stream function.
type ModelOptions struct {
	Model         *aitypes.Model
	MaxTokens     *int
	ContextWindow *int
	ThinkingLevel agenttypes.ThinkingLevel
	APIKey        func(context.Context, string) (string, error)
	StreamFn      agenttypes.StreamFn
}

// builtinStreamFnForModel returns the native provider stream function for a
// model's API. Upstream createAgentSession lets callers omit a stream function
// and falls back to the registered provider for the model's API; the Go port
// resolves that fallback directly from the accepted Pith AI API modules.
//
// The returned closure is bound to this model's API and holds no process-wide
// state, so independent sessions and providers never interfere. An API that
// has no Go implementation returns nil, which surfaces as a construction error
// instead of a nil dereference during a run.
func builtinStreamFnForModel(model *aitypes.Model) agenttypes.StreamFn {
	if model == nil {
		return nil
	}
	var streams aitypes.ProviderStreams
	switch model.Api {
	case aitypes.ApiOpenAICompletions:
		streams = api.OpenAICompletionsApi()
	case aitypes.ApiOpenAIResponses:
		streams = api.OpenAIResponsesApi()
	case aitypes.ApiOpenAICodexResponses:
		streams = api.OpenAICodexResponsesApi()
	case aitypes.ApiAzureOpenAIResponses:
		streams = api.AzureOpenAIResponsesApi()
	case aitypes.ApiAnthropicMessages:
		streams = api.AnthropicMessagesApi()
	case aitypes.ApiGoogleGenerativeAI:
		streams = api.GoogleGenerativeAIApi()
	case aitypes.ApiGoogleVertex:
		streams = api.GoogleVertexApi()
	case aitypes.ApiMistralConversations:
		streams = api.MistralConversationsApi()
	case aitypes.ApiBedrockConverseStream:
		streams = api.BedrockConverseStreamApi()
	case aitypes.ApiPiMessages:
		streams = api.PiMessagesApi()
	default:
		return nil
	}
	if streams == nil {
		return nil
	}
	return func(m *aitypes.Model, ctx *aitypes.TranscriptContext, options *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		return streams.StreamSimple(m, ctx, options)
	}
}

func cloneModel(model *aitypes.Model) (*aitypes.Model, error) {
	if model == nil {
		return nil, errors.New("no model specified")
	}
	data, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("copy model: %w", err)
	}
	var clone aitypes.Model
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, fmt.Errorf("copy model: %w", err)
	}
	return &clone, nil
}

// ResolveModel validates caller overrides and returns an independent copy of
// the model. It preserves provider, API, base URL, headers, compatibility and
// the model's own context/output capacity; it never substitutes a hardcoded
// 128K/4096 default.
func ResolveModel(options ModelOptions) (*aitypes.Model, error) {
	resolved, err := cloneModel(options.Model)
	if err != nil {
		return nil, err
	}
	if options.MaxTokens != nil {
		if *options.MaxTokens <= 0 {
			return nil, fmt.Errorf("max tokens must be positive, got %d", *options.MaxTokens)
		}
		if resolved.MaxTokens > 0 && float64(*options.MaxTokens) > resolved.MaxTokens {
			return nil, fmt.Errorf("max tokens %d exceeds output capacity %g of %s/%s", *options.MaxTokens, resolved.MaxTokens, resolved.Provider, resolved.Id)
		}
		resolved.MaxTokens = float64(*options.MaxTokens)
	}
	if options.ContextWindow != nil {
		if *options.ContextWindow <= 0 {
			return nil, fmt.Errorf("context window must be positive, got %d", *options.ContextWindow)
		}
		if resolved.ContextWindow > 0 && float64(*options.ContextWindow) > resolved.ContextWindow {
			return nil, fmt.Errorf("context window %d exceeds context capacity %g of %s/%s", *options.ContextWindow, resolved.ContextWindow, resolved.Provider, resolved.Id)
		}
		resolved.ContextWindow = float64(*options.ContextWindow)
	}
	if level := options.ThinkingLevel; level != "" && level != agenttypes.ThinkingOff {
		// The session applies the requested level; marking reasoning keeps the
		// model's thinking capability visible to downstream requests.
		resolved.Reasoning = true
	}
	return resolved, nil
}

// ---------------------------------------------------------------------------
// Model resolution
// ---------------------------------------------------------------------------

// ScopedModel is a resolved model plus an optional explicit thinking level.
type ScopedModel struct {
	Model         *aitypes.Model
	ThinkingLevel agenttypes.ThinkingLevel
}

// ParsedModelResult is the result of parsing one model pattern.
type ParsedModelResult struct {
	Model         *aitypes.Model
	ThinkingLevel agenttypes.ThinkingLevel
	Warning       string
}

// ParseModelPatternOptions controls invalid thinking-level handling.
type ParseModelPatternOptions struct {
	AllowInvalidThinkingLevelFallback *bool
}

func modelAlias(id string) bool {
	if strings.HasSuffix(id, "-latest") {
		return true
	}
	return !regexp.MustCompile(`-\d{8}$`).MatchString(id)
}

func compareModels(a, b aitypes.Model) bool {
	return a.Provider == b.Provider && a.Id == b.Id
}

// FindExactModelReferenceMatch matches a bare id or canonical
// provider/modelId reference. Ambiguous bare ids are rejected.
func FindExactModelReferenceMatch(reference string, available []aitypes.Model) *aitypes.Model {
	trimmed := strings.TrimSpace(reference)
	if trimmed == "" {
		return nil
	}
	normalized := strings.ToLower(trimmed)

	var canonical []aitypes.Model
	for _, model := range available {
		if strings.ToLower(string(model.Provider)+"/"+model.Id) == normalized {
			canonical = append(canonical, model)
		}
	}
	if len(canonical) == 1 {
		return modelPointer(canonical[0])
	}
	if len(canonical) > 1 {
		return nil
	}

	if slash := strings.Index(trimmed, "/"); slash != -1 {
		provider := strings.TrimSpace(trimmed[:slash])
		modelID := strings.TrimSpace(trimmed[slash+1:])
		if provider != "" && modelID != "" {
			var providerMatches []aitypes.Model
			for _, model := range available {
				if strings.ToLower(string(model.Provider)) == strings.ToLower(provider) && strings.ToLower(model.Id) == strings.ToLower(modelID) {
					providerMatches = append(providerMatches, model)
				}
			}
			if len(providerMatches) == 1 {
				return modelPointer(providerMatches[0])
			}
			if len(providerMatches) > 1 {
				return nil
			}
		}
	}

	var idMatches []aitypes.Model
	for _, model := range available {
		if strings.ToLower(model.Id) == normalized {
			idMatches = append(idMatches, model)
		}
	}
	if len(idMatches) == 1 {
		return modelPointer(idMatches[0])
	}
	return nil
}

func modelPointer(model aitypes.Model) *aitypes.Model {
	copy := model
	return &copy
}

func tryMatchModel(pattern string, available []aitypes.Model) *aitypes.Model {
	if exact := FindExactModelReferenceMatch(pattern, available); exact != nil {
		return exact
	}
	lower := strings.ToLower(pattern)
	var matches []aitypes.Model
	for _, model := range available {
		if strings.Contains(strings.ToLower(model.Id), lower) || strings.Contains(strings.ToLower(model.Name), lower) {
			matches = append(matches, model)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	var aliases, dated []aitypes.Model
	for _, model := range matches {
		if modelAlias(model.Id) {
			aliases = append(aliases, model)
		} else {
			dated = append(dated, model)
		}
	}
	if len(aliases) > 0 {
		sort.Slice(aliases, func(i, j int) bool { return aliases[i].Id > aliases[j].Id })
		return modelPointer(aliases[0])
	}
	sort.Slice(dated, func(i, j int) bool { return dated[i].Id > dated[j].Id })
	return modelPointer(dated[0])
}

// ParseModelPattern parses "pattern:level", tolerating colons in model ids.
func ParseModelPattern(pattern string, available []aitypes.Model, options *ParseModelPatternOptions) ParsedModelResult {
	if exact := tryMatchModel(pattern, available); exact != nil {
		return ParsedModelResult{Model: exact}
	}
	lastColon := strings.LastIndex(pattern, ":")
	if lastColon == -1 {
		return ParsedModelResult{}
	}
	prefix, suffix := pattern[:lastColon], pattern[lastColon+1:]
	if isValidThinkingLevel(suffix) {
		result := ParseModelPattern(prefix, available, options)
		if result.Model != nil {
			if result.Warning != "" {
				return ParsedModelResult{Model: result.Model, Warning: result.Warning}
			}
			return ParsedModelResult{Model: result.Model, ThinkingLevel: agenttypes.ThinkingLevel(suffix)}
		}
		return result
	}
	allowFallback := true
	if options != nil && options.AllowInvalidThinkingLevelFallback != nil {
		allowFallback = *options.AllowInvalidThinkingLevelFallback
	}
	if !allowFallback {
		return ParsedModelResult{}
	}
	result := ParseModelPattern(prefix, available, options)
	if result.Model != nil {
		return ParsedModelResult{
			Model:   result.Model,
			Warning: fmt.Sprintf("Invalid thinking level %q in pattern %q. Using default instead.", suffix, pattern),
		}
	}
	return result
}

// ModelScopeDiagnostic is a non-fatal model-scope warning.
type ModelScopeDiagnostic struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Pattern string `json:"pattern"`
}

// ResolveModelScopeResult is the resolved scope plus diagnostics.
type ResolveModelScopeResult struct {
	ScopedModels []ScopedModel
	Diagnostics  []ModelScopeDiagnostic
}

// ResolveModelScopeFromModels resolves patterns against a supplied model list.
func ResolveModelScopeFromModels(patterns []string, models []aitypes.Model) ResolveModelScopeResult {
	available := append([]aitypes.Model(nil), models...)
	result := ResolveModelScopeResult{}
	appendModel := func(model *aitypes.Model, level agenttypes.ThinkingLevel) {
		if model == nil {
			return
		}
		for _, existing := range result.ScopedModels {
			if compareModels(*existing.Model, *model) {
				return
			}
		}
		result.ScopedModels = append(result.ScopedModels, ScopedModel{Model: model, ThinkingLevel: level})
	}

	for _, pattern := range patterns {
		if strings.ContainsAny(pattern, "*?[") {
			globPattern := pattern
			var level agenttypes.ThinkingLevel
			if colon := strings.LastIndex(pattern, ":"); colon != -1 {
				if suffix := pattern[colon+1:]; isValidThinkingLevel(suffix) {
					level = agenttypes.ThinkingLevel(suffix)
					globPattern = pattern[:colon]
				}
			}
			if exact := FindExactModelReferenceMatch(globPattern, available); exact != nil {
				appendModel(exact, level)
				continue
			}
			var matched []aitypes.Model
			for _, model := range available {
				full := string(model.Provider) + "/" + model.Id
				if globMatch(globPattern, full) || globMatch(globPattern, model.Id) {
					matched = append(matched, model)
				}
			}
			if len(matched) == 0 {
				result.Diagnostics = append(result.Diagnostics, ModelScopeDiagnostic{Type: "warning", Code: "no-match", Message: fmt.Sprintf("No models match pattern %q", pattern), Pattern: pattern})
				continue
			}
			for i := range matched {
				appendModel(&matched[i], level)
			}
			continue
		}

		parsed := ParseModelPattern(pattern, available, nil)
		if parsed.Warning != "" {
			result.Diagnostics = append(result.Diagnostics, ModelScopeDiagnostic{Type: "warning", Code: "invalid-thinking-level", Message: parsed.Warning, Pattern: pattern})
		}
		if parsed.Model == nil {
			result.Diagnostics = append(result.Diagnostics, ModelScopeDiagnostic{Type: "warning", Code: "no-match", Message: fmt.Sprintf("No models match pattern %q", pattern), Pattern: pattern})
			continue
		}
		appendModel(parsed.Model, parsed.ThinkingLevel)
	}
	return result
}

// ResolveModelScopeWithDiagnostics resolves scope patterns against the runtime's
// available models.
func ResolveModelScopeWithDiagnostics(patterns []string, runtime *ModelRuntime, options *authtypes.AuthOperationOptions) (ResolveModelScopeResult, error) {
	models, err := runtime.GetAvailable(context.Background(), options)
	if err != nil {
		return ResolveModelScopeResult{}, err
	}
	return ResolveModelScopeFromModels(patterns, models), nil
}

// ResolveModelScope resolves scope patterns and drops diagnostics.
func ResolveModelScope(patterns []string, runtime *ModelRuntime, options *authtypes.AuthOperationOptions) ([]ScopedModel, error) {
	result, err := ResolveModelScopeWithDiagnostics(patterns, runtime, options)
	if err != nil {
		return nil, err
	}
	return result.ScopedModels, nil
}

// ResolveCliModelOptions are the CLI inputs for single-model resolution.
type ResolveCliModelOptions struct {
	CLIProvider  string
	CLIModel     string
	CLIThinking  agenttypes.ThinkingLevel
	ModelRuntime *ModelRuntime
}

// ResolveCliModelResult is a resolved CLI model plus diagnostics.
type ResolveCliModelResult struct {
	Model         *aitypes.Model
	ThinkingLevel agenttypes.ThinkingLevel
	Warning       string
	Error         string
}

func buildFallbackModel(provider, modelID string, available []aitypes.Model) *aitypes.Model {
	var providerModels []aitypes.Model
	for _, model := range available {
		if string(model.Provider) == provider {
			providerModels = append(providerModels, model)
		}
	}
	if len(providerModels) == 0 {
		return nil
	}
	base := providerModels[0]
	if defaultID, ok := DefaultModelPerProvider[provider]; ok {
		for _, model := range providerModels {
			if model.Id == defaultID {
				base = model
				break
			}
		}
	}
	base.Id = modelID
	base.Name = modelID
	return modelPointer(base)
}

// ResolveCliModel resolves one model from CLI-style inputs.
func ResolveCliModel(options ResolveCliModelOptions) ResolveCliModelResult {
	if options.CLIModel == "" || options.ModelRuntime == nil {
		return ResolveCliModelResult{}
	}
	available := options.ModelRuntime.GetModels()
	if len(available) == 0 {
		return ResolveCliModelResult{Error: "No models available. Check your installation or add models to models.json."}
	}
	providerIDs := map[string]string{}
	for _, model := range available {
		providerIDs[strings.ToLower(string(model.Provider))] = string(model.Provider)
	}
	provider := ""
	if options.CLIProvider != "" {
		canonical, ok := providerIDs[strings.ToLower(options.CLIProvider)]
		if !ok {
			return ResolveCliModelResult{Error: fmt.Sprintf("Unknown provider %q. Use --list-models to see available providers/models.", options.CLIProvider)}
		}
		provider = canonical
	}

	pattern := options.CLIModel
	inferred := false
	if provider == "" {
		if slash := strings.Index(options.CLIModel, "/"); slash != -1 {
			if canonical, ok := providerIDs[strings.ToLower(options.CLIModel[:slash])]; ok {
				provider = canonical
				pattern = options.CLIModel[slash+1:]
				inferred = true
			}
		}
	}

	if provider == "" {
		lower := strings.ToLower(options.CLIModel)
		var exact []aitypes.Model
		for _, model := range available {
			if strings.ToLower(model.Id) == lower || strings.ToLower(string(model.Provider)+"/"+model.Id) == lower {
				exact = append(exact, model)
			}
		}
		if len(exact) == 1 {
			return ResolveCliModelResult{Model: modelPointer(exact[0])}
		}
		if len(exact) > 1 {
			var matches []string
			for _, model := range exact {
				matches = append(matches, string(model.Provider)+"/"+model.Id)
			}
			sort.Strings(matches)
			return ResolveCliModelResult{Error: fmt.Sprintf("Model %q is ambiguous across providers: %s. Use --provider or provider/model.", options.CLIModel, strings.Join(matches, ", "))}
		}
	}

	if options.CLIProvider != "" && provider != "" {
		prefix := provider + "/"
		if strings.HasPrefix(strings.ToLower(options.CLIModel), strings.ToLower(prefix)) {
			pattern = options.CLIModel[len(prefix):]
		}
	}

	candidates := available
	if provider != "" {
		candidates = nil
		for _, model := range available {
			if string(model.Provider) == provider {
				candidates = append(candidates, model)
			}
		}
	}
	parsed := ParseModelPattern(pattern, candidates, &ParseModelPatternOptions{AllowInvalidThinkingLevelFallback: boolPointer(false)})
	if parsed.Model != nil {
		if inferred && !options.ModelRuntime.HasConfiguredAuth(string(parsed.Model.Provider)) {
			lower := strings.ToLower(options.CLIModel)
			for i := range available {
				if strings.ToLower(available[i].Id) == lower && !compareModels(available[i], *parsed.Model) && options.ModelRuntime.HasConfiguredAuth(string(available[i].Provider)) {
					return ResolveCliModelResult{Model: modelPointer(available[i])}
				}
			}
		}
		return ResolveCliModelResult{Model: parsed.Model, ThinkingLevel: parsed.ThinkingLevel, Warning: parsed.Warning}
	}

	if inferred {
		lower := strings.ToLower(options.CLIModel)
		for i := range available {
			if strings.ToLower(available[i].Id) == lower || strings.ToLower(string(available[i].Provider)+"/"+available[i].Id) == lower {
				return ResolveCliModelResult{Model: modelPointer(available[i])}
			}
		}
		fallback := ParseModelPattern(options.CLIModel, available, &ParseModelPatternOptions{AllowInvalidThinkingLevelFallback: boolPointer(false)})
		if fallback.Model != nil {
			return ResolveCliModelResult{Model: fallback.Model, ThinkingLevel: fallback.ThinkingLevel, Warning: fallback.Warning}
		}
	}

	if provider != "" {
		fallbackPattern := pattern
		var fallbackThinking agenttypes.ThinkingLevel
		if options.CLIThinking == "" {
			if colon := strings.LastIndex(pattern, ":"); colon != -1 {
				if suffix := pattern[colon+1:]; isValidThinkingLevel(suffix) {
					fallbackPattern = pattern[:colon]
					fallbackThinking = agenttypes.ThinkingLevel(suffix)
				}
			}
		}
		if fallbackModel := buildFallbackModel(provider, fallbackPattern, available); fallbackModel != nil {
			requested := options.CLIThinking
			if requested == "" {
				requested = fallbackThinking
			}
			if requested != "" && requested != agenttypes.ThinkingOff {
				fallbackModel.Reasoning = true
			}
			warning := parsed.Warning
			if warning != "" {
				warning += " "
			}
			warning += fmt.Sprintf("Model %q not found for provider %q. Using custom model id.", fallbackPattern, provider)
			return ResolveCliModelResult{Model: fallbackModel, ThinkingLevel: fallbackThinking, Warning: warning}
		}
	}

	display := options.CLIModel
	if provider != "" {
		display = provider + "/" + pattern
	}
	return ResolveCliModelResult{Warning: parsed.Warning, Error: fmt.Sprintf("Model %q not found. Use --list-models to see available models.", display)}
}

func boolPointer(value bool) *bool { return &value }

// InitialModelResult is the model chosen for a new session.
type InitialModelResult struct {
	Model           *aitypes.Model
	ThinkingLevel   agenttypes.ThinkingLevel
	FallbackMessage string
}

// FindInitialModelOptions are the inputs for initial model selection.
type FindInitialModelOptions struct {
	CLIProvider          string
	CLIModel             string
	ScopedModels         []ScopedModel
	IsContinuing         bool
	DefaultProvider      string
	DefaultModelID       string
	DefaultThinkingLevel agenttypes.ThinkingLevel
	ModelThinkingLevels  map[string]agenttypes.ThinkingLevel
	ModelRuntime         *ModelRuntime
}

// FindInitialModel chooses the initial model following the documented priority.
func FindInitialModel(options FindInitialModelOptions) InitialModelResult {
	if options.ModelRuntime == nil {
		return InitialModelResult{ThinkingLevel: DefaultThinkingLevel}
	}
	thinking := DefaultThinkingLevel
	if options.DefaultThinkingLevel != "" {
		thinking = options.DefaultThinkingLevel
	}

	if options.CLIProvider != "" && options.CLIModel != "" {
		resolved := ResolveCliModel(ResolveCliModelOptions{
			CLIProvider:  options.CLIProvider,
			CLIModel:     options.CLIModel,
			ModelRuntime: options.ModelRuntime,
		})
		if resolved.Model != nil {
			return InitialModelResult{Model: resolved.Model, ThinkingLevel: DefaultThinkingLevel}
		}
	}

	if len(options.ScopedModels) > 0 && !options.IsContinuing {
		scoped := options.ScopedModels[0]
		level := scoped.ThinkingLevel
		key := string(scoped.Model.Provider) + "/" + scoped.Model.Id
		if level == "" {
			level = options.ModelThinkingLevels[key]
		}
		if level == "" {
			level = thinking
		}
		return InitialModelResult{Model: scoped.Model, ThinkingLevel: level}
	}

	if options.DefaultProvider != "" && options.DefaultModelID != "" {
		if found := options.ModelRuntime.GetModel(options.DefaultProvider, options.DefaultModelID); found != nil && options.ModelRuntime.HasConfiguredAuth(string(found.Provider)) {
			level := options.ModelThinkingLevels[options.DefaultProvider+"/"+options.DefaultModelID]
			if level == "" {
				level = thinking
			}
			return InitialModelResult{Model: found, ThinkingLevel: level}
		}
	}

	available := options.ModelRuntime.GetAvailableSnapshot()
	if len(available) > 0 {
		providerOrder := make([]string, 0, len(DefaultModelPerProvider))
		for provider := range DefaultModelPerProvider {
			providerOrder = append(providerOrder, provider)
		}
		sort.Strings(providerOrder)
		for _, provider := range providerOrder {
			defaultID := DefaultModelPerProvider[provider]
			for i := range available {
				if string(available[i].Provider) == provider && available[i].Id == defaultID {
					return InitialModelResult{Model: modelPointer(available[i]), ThinkingLevel: DefaultThinkingLevel}
				}
			}
		}
		return InitialModelResult{Model: modelPointer(available[0]), ThinkingLevel: DefaultThinkingLevel}
	}

	return InitialModelResult{ThinkingLevel: DefaultThinkingLevel}
}

// RestoreModelFromSession restores a saved model or falls back to an available
// one.
func RestoreModelFromSession(savedProvider, savedModelID string, currentModel *aitypes.Model, runtime *ModelRuntime) (*aitypes.Model, string) {
	if runtime == nil {
		return currentModel, ""
	}
	restored := runtime.GetModel(savedProvider, savedModelID)
	if restored != nil && runtime.HasConfiguredAuth(string(restored.Provider)) {
		return restored, ""
	}
	reason := "model no longer exists"
	if restored != nil {
		reason = "no auth configured"
	}
	if currentModel != nil {
		return currentModel, fmt.Sprintf("Could not restore model %s/%s (%s). Using %s/%s.", savedProvider, savedModelID, reason, currentModel.Provider, currentModel.Id)
	}
	available := runtime.GetAvailableSnapshot()
	if len(available) > 0 {
		fallback := &available[0]
		for provider, defaultID := range DefaultModelPerProvider {
			for i := range available {
				if string(available[i].Provider) == provider && available[i].Id == defaultID {
					fallback = &available[i]
					break
				}
			}
		}
		return fallback, fmt.Sprintf("Could not restore model %s/%s (%s). Using %s/%s.", savedProvider, savedModelID, reason, fallback.Provider, fallback.Id)
	}
	return nil, ""
}

// ---------------------------------------------------------------------------
// Model config (models.json)
// ---------------------------------------------------------------------------

// ModelsJsonModel is one custom model definition from models.json.
type ModelsJsonModel struct {
	ID               string                       `json:"id"`
	Name             string                       `json:"name,omitempty"`
	API              aitypes.Api                  `json:"api,omitempty"`
	BaseURL          string                       `json:"baseUrl,omitempty"`
	Reasoning        *bool                        `json:"reasoning,omitempty"`
	ThinkingLevelMap aitypes.ThinkingLevelMap     `json:"thinkingLevelMap,omitempty"`
	Input            []aitypes.ModelInputModality `json:"input,omitempty"`
	InputLimits      *aitypes.ModelInputLimits    `json:"inputLimits,omitempty"`
	Cost             *aitypes.ModelCost           `json:"cost,omitempty"`
	PromptCache      *aitypes.ModelPromptCache    `json:"promptCache,omitempty"`
	ContextWindow    *float64                     `json:"contextWindow,omitempty"`
	MaxTokens        *float64                     `json:"maxTokens,omitempty"`
	SamplingParams   map[string]any               `json:"samplingParams,omitempty"`
	Headers          map[string]string            `json:"headers,omitempty"`
	Compat           json.RawMessage              `json:"compat,omitempty"`
}

// ModelsJsonModelOverride overrides one existing model from models.json.
type ModelsJsonModelOverride struct {
	Name             string                       `json:"name,omitempty"`
	Reasoning        *bool                        `json:"reasoning,omitempty"`
	ThinkingLevelMap aitypes.ThinkingLevelMap     `json:"thinkingLevelMap,omitempty"`
	Input            []aitypes.ModelInputModality `json:"input,omitempty"`
	InputLimits      *aitypes.ModelInputLimits    `json:"inputLimits,omitempty"`
	Cost             *aitypes.ModelCost           `json:"cost,omitempty"`
	PromptCache      *aitypes.ModelPromptCache    `json:"promptCache,omitempty"`
	ContextWindow    *float64                     `json:"contextWindow,omitempty"`
	MaxTokens        *float64                     `json:"maxTokens,omitempty"`
	SamplingParams   map[string]any               `json:"samplingParams,omitempty"`
	Headers          map[string]string            `json:"headers,omitempty"`
	Compat           json.RawMessage              `json:"compat,omitempty"`
}

// ModelsJsonProvider is one provider entry from models.json.
type ModelsJsonProvider struct {
	Name           string                             `json:"name,omitempty"`
	BaseURL        string                             `json:"baseUrl,omitempty"`
	APIKey         string                             `json:"apiKey,omitempty"`
	API            aitypes.Api                        `json:"api,omitempty"`
	OAuth          string                             `json:"oauth,omitempty"`
	Headers        map[string]string                  `json:"headers,omitempty"`
	Compat         json.RawMessage                    `json:"compat,omitempty"`
	AuthHeader     *bool                              `json:"authHeader,omitempty"`
	Models         []ModelsJsonModel                  `json:"models,omitempty"`
	ModelOverrides map[string]ModelsJsonModelOverride `json:"modelOverrides,omitempty"`
}

type modelsConfigDocument struct {
	Providers map[string]ModelsJsonProvider `json:"providers"`
}

// ModelConfig is one immutable, credential-blind models.json snapshot.
type ModelConfig struct {
	providers map[string]ModelsJsonProvider
	err       string
}

// LoadModelConfig reads models.json. A missing file is an empty config; a
// malformed file records the error but never throws.
func LoadModelConfig(modelsPath string) (*ModelConfig, error) {
	config := &ModelConfig{providers: map[string]ModelsJsonProvider{}}
	if modelsPath == "" {
		return config, nil
	}
	data, err := os.ReadFile(modelsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config, nil
		}
		config.err = fmt.Sprintf("Failed to load models.json: %v\n\nFile: %s", err, modelsPath)
		return config, nil
	}
	var document modelsConfigDocument
	if err := json.Unmarshal(stripBOM(data), &document); err != nil {
		config.err = fmt.Sprintf("Failed to parse models.json: %v\n\nFile: %s", err, modelsPath)
		return config, nil
	}
	for id, provider := range document.Providers {
		config.providers[id] = provider
	}
	return config, nil
}

// GetProvider returns one provider config.
func (c *ModelConfig) GetProvider(providerID string) (ModelsJsonProvider, bool) {
	if c == nil {
		return ModelsJsonProvider{}, false
	}
	provider, ok := c.providers[providerID]
	return provider, ok
}

// GetProviderIDs returns provider ids in stable order.
func (c *ModelConfig) GetProviderIDs() []string {
	if c == nil {
		return nil
	}
	ids := make([]string, 0, len(c.providers))
	for id := range c.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// GetError returns the load/parse error, if any.
func (c *ModelConfig) GetError() string {
	if c == nil {
		return ""
	}
	return c.err
}

func (c *ModelConfig) providerPointer(providerID string) *ModelsJsonProvider {
	provider, ok := c.GetProvider(providerID)
	if !ok {
		return nil
	}
	return &provider
}

// ---------------------------------------------------------------------------
// Models stores
// ---------------------------------------------------------------------------

// InMemoryCodingAgentModelsStore is the in-memory provider-catalog store.
type InMemoryCodingAgentModelsStore struct {
	*ai.InMemoryModelsStore
}

// NewInMemoryCodingAgentModelsStore builds an empty store.
func NewInMemoryCodingAgentModelsStore() *InMemoryCodingAgentModelsStore {
	return &InMemoryCodingAgentModelsStore{InMemoryModelsStore: ai.NewInMemoryModelsStore()}
}

// FileModelsStore is JSON-file-backed provider-catalog storage.
type FileModelsStore struct {
	Path    string
	backend *FileAuthStorageBackend
}

// NewFileModelsStore builds file-backed catalog storage.
func NewFileModelsStore(path string) *FileModelsStore {
	return &FileModelsStore{Path: path, backend: NewFileAuthStorageBackend(path)}
}

func (s *FileModelsStore) readAll(ctx context.Context, options *ai.ModelsStoreOperationOptions) (map[string]json.RawMessage, error) {
	if err := modelsStoreSignal(ctx, options); err != nil {
		return nil, err
	}
	data := map[string]json.RawMessage{}
	err := s.backend.WithLock(func(current []byte) ([]byte, error) {
		if len(strings.TrimSpace(string(current))) == 0 {
			return nil, nil
		}
		return nil, json.Unmarshal(stripBOM(current), &data)
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Read returns a stored entry clone.
func (s *FileModelsStore) Read(ctx context.Context, providerID string, options *ai.ModelsStoreOperationOptions) (*ai.ModelsStoreEntry, error) {
	data, err := s.readAll(ctx, options)
	if err != nil {
		return nil, err
	}
	raw, ok := data[providerID]
	if !ok {
		return nil, nil
	}
	var entry ai.ModelsStoreEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

// Write stores an entry.
func (s *FileModelsStore) Write(ctx context.Context, providerID string, entry *ai.ModelsStoreEntry, options *ai.ModelsStoreOperationOptions) error {
	if err := modelsStoreSignal(ctx, options); err != nil {
		return err
	}
	return s.backend.WithLock(func(current []byte) ([]byte, error) {
		data := map[string]json.RawMessage{}
		if len(strings.TrimSpace(string(current))) > 0 {
			if err := json.Unmarshal(stripBOM(current), &data); err != nil {
				return nil, err
			}
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		data[providerID] = encoded
		return json.MarshalIndent(data, "", "  ")
	})
}

// Delete removes an entry.
func (s *FileModelsStore) Delete(ctx context.Context, providerID string, options *ai.ModelsStoreOperationOptions) error {
	if err := modelsStoreSignal(ctx, options); err != nil {
		return err
	}
	return s.backend.WithLock(func(current []byte) ([]byte, error) {
		data := map[string]json.RawMessage{}
		if len(strings.TrimSpace(string(current))) > 0 {
			if err := json.Unmarshal(stripBOM(current), &data); err != nil {
				return nil, err
			}
		}
		delete(data, providerID)
		return json.MarshalIndent(data, "", "  ")
	})
}

func modelsStoreSignal(ctx context.Context, options *ai.ModelsStoreOperationOptions) error {
	if options != nil && options.Signal != nil {
		return options.Signal.Err()
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Provider composition
// ---------------------------------------------------------------------------

// ExtensionOAuthConfig is an extension-provided OAuth auth method. Login uses
// the caller's interaction callback; the SDK never opens a browser itself.
type ExtensionOAuthConfig struct {
	Name           string
	IsSubscription bool
	Login          func(ctx context.Context, interaction authtypes.ProviderAuthInteraction) (*authtypes.OAuthCredential, error)
	RefreshToken   func(ctx context.Context, credential *authtypes.OAuthCredential) (*authtypes.OAuthCredential, error)
	GetAPIKey      func(credential *authtypes.OAuthCredential) string
	ModifyModels   func(models []aitypes.Model, credential *authtypes.OAuthCredential) []aitypes.Model
}

// ProviderConfigInput is an extension-registered provider configuration. It is
// the single owner of the type shared by model-registry and provider-composer.
type ProviderConfigInput struct {
	Name          string
	BaseURL       string
	APIKey        string
	API           aitypes.Api
	StreamSimple  agenttypes.StreamFn
	Headers       map[string]string
	AuthHeader    bool
	OAuth         *ExtensionOAuthConfig
	Models        []ModelsJsonModel
	RefreshModels func(ctx context.Context, refresh *ai.RefreshModelsContext) ([]ModelsJsonModel, error)
}

// AuthStatus is a human-readable provider auth status.
type AuthStatus struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source,omitempty"`
	Label      string `json:"label,omitempty"`
}

// ResolvedRequestAuth is the request-time auth resolution for one model.
type ResolvedRequestAuth struct {
	OK      bool
	APIKey  *string
	Headers aitypes.ProviderHeaders
	BaseURL *string
	Env     map[string]string
	Error   string
}

// CompatibilityRequestConfig is the plugin-compatibility request surface.
type CompatibilityRequestConfig struct {
	Headers    aitypes.ProviderHeaders
	AuthHeader bool
}

// ClearAPIKeyCache clears cached configured API-key command results.
func ClearAPIKeyCache() {
	ClearConfigValueCache()
}

func configuredAPIKey(config *ModelsJsonProvider, extension *ProviderConfigInput) string {
	if extension != nil && extension.APIKey != "" {
		return extension.APIKey
	}
	if config != nil {
		return config.APIKey
	}
	return ""
}

func configuredHeaders(config *ModelsJsonProvider, extension *ProviderConfigInput) map[string]string {
	headers := map[string]string{}
	if config != nil {
		for key, value := range config.Headers {
			headers[key] = value
		}
	}
	if extension != nil {
		for key, value := range extension.Headers {
			headers[key] = value
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// ValidateExtensionProvider validates an extension provider config eagerly.
func ValidateExtensionProvider(providerID string, config ProviderConfigInput) error {
	if config.StreamSimple != nil && config.API == "" {
		return fmt.Errorf("provider %s: \"api\" is required when registering streamSimple", providerID)
	}
	for _, model := range config.Models {
		if model.ID == "" {
			return fmt.Errorf("provider %s: model id is required", providerID)
		}
	}
	return nil
}

type providerStreamsAdapter struct {
	provider ai.Provider
}

func (a *providerStreamsAdapter) Stream(model *aitypes.Model, ctx *aitypes.TranscriptContext, options *aitypes.StreamOptions) *aitypes.AssistantMessageEventStream {
	return a.provider.Stream(*model, ctx, options)
}

func (a *providerStreamsAdapter) StreamSimple(model *aitypes.Model, ctx *aitypes.TranscriptContext, options *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
	return a.provider.StreamSimple(*model, ctx, options)
}

func (a *providerStreamsAdapter) FetchDeferred(*aitypes.Model, aitypes.DeferredHandle, *aitypes.DeferredFetchOptions) (*aitypes.AssistantMessageEventStream, error) {
	return nil, errors.New("deferred responses are not supported")
}

func (a *providerStreamsAdapter) CancelDeferred(*aitypes.Model, aitypes.DeferredHandle, *aitypes.DeferredCancelOptions) error {
	return errors.New("deferred responses are not supported")
}

type streamFnAdapter struct {
	fn agenttypes.StreamFn
}

func (a *streamFnAdapter) Stream(model *aitypes.Model, ctx *aitypes.TranscriptContext, options *aitypes.StreamOptions) *aitypes.AssistantMessageEventStream {
	simple := &aitypes.SimpleStreamOptions{}
	if options != nil {
		simple.StreamOptions = *options
	}
	return a.fn(model, ctx, simple)
}

func (a *streamFnAdapter) StreamSimple(model *aitypes.Model, ctx *aitypes.TranscriptContext, options *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
	return a.fn(model, ctx, options)
}

func (a *streamFnAdapter) FetchDeferred(*aitypes.Model, aitypes.DeferredHandle, *aitypes.DeferredFetchOptions) (*aitypes.AssistantMessageEventStream, error) {
	return nil, errors.New("deferred responses are not supported")
}

func (a *streamFnAdapter) CancelDeferred(*aitypes.Model, aitypes.DeferredHandle, *aitypes.DeferredCancelOptions) error {
	return errors.New("deferred responses are not supported")
}

func modelFromJSON(providerID string, definition ModelsJsonModel, provider ModelsJsonProvider, defaults *aitypes.Model) (aitypes.Model, error) {
	api := definition.API
	if api == "" {
		api = provider.API
	}
	if api == "" && defaults != nil {
		api = defaults.Api
	}
	if api == "" {
		return aitypes.Model{}, fmt.Errorf("provider %s, model %s: no \"api\" specified", providerID, definition.ID)
	}
	baseURL := definition.BaseURL
	if baseURL == "" {
		baseURL = provider.BaseURL
	}
	if baseURL == "" && defaults != nil {
		baseURL = defaults.BaseUrl
	}
	if baseURL == "" {
		return aitypes.Model{}, fmt.Errorf("provider %s: \"baseUrl\" is required when defining custom models", providerID)
	}
	if definition.ContextWindow != nil && *definition.ContextWindow <= 0 {
		return aitypes.Model{}, fmt.Errorf("provider %s, model %s: invalid contextWindow", providerID, definition.ID)
	}
	if definition.MaxTokens != nil && *definition.MaxTokens <= 0 {
		return aitypes.Model{}, fmt.Errorf("provider %s, model %s: invalid maxTokens", providerID, definition.ID)
	}
	model := aitypes.Model{
		Id:               definition.ID,
		Name:             definition.Name,
		Api:              api,
		Provider:         aitypes.ProviderId(providerID),
		BaseUrl:          baseURL,
		Reasoning:        false,
		ThinkingLevelMap: definition.ThinkingLevelMap,
		Input:            definition.Input,
		ContextWindow:    128000,
		MaxTokens:        16384,
	}
	if model.Name == "" {
		model.Name = definition.ID
	}
	if definition.Reasoning != nil {
		model.Reasoning = *definition.Reasoning
	}
	if len(model.Input) == 0 {
		model.Input = []aitypes.ModelInputModality{aitypes.ModelInputText}
	}
	if definition.Cost != nil {
		model.Cost = *definition.Cost
	}
	model.PromptCache = definition.PromptCache
	if definition.ContextWindow != nil {
		model.ContextWindow = *definition.ContextWindow
	}
	if definition.MaxTokens != nil {
		model.MaxTokens = *definition.MaxTokens
	}
	model.SamplingParams = definition.SamplingParams
	if defaults != nil && definition.Cost == nil {
		model.Cost = defaults.Cost
	}
	return model, nil
}

func applyModelOverride(model aitypes.Model, override ModelsJsonModelOverride) aitypes.Model {
	if override.Name != "" {
		model.Name = override.Name
	}
	if override.Reasoning != nil {
		model.Reasoning = *override.Reasoning
	}
	if override.ThinkingLevelMap != nil {
		model.ThinkingLevelMap = override.ThinkingLevelMap
	}
	if override.Input != nil {
		model.Input = override.Input
	}
	if override.InputLimits != nil {
		model.InputLimits = override.InputLimits
	}
	if override.Cost != nil {
		model.Cost = *override.Cost
	}
	if override.PromptCache != nil {
		model.PromptCache = override.PromptCache
	}
	if override.ContextWindow != nil {
		model.ContextWindow = *override.ContextWindow
	}
	if override.MaxTokens != nil {
		model.MaxTokens = *override.MaxTokens
	}
	if override.SamplingParams != nil {
		model.SamplingParams = override.SamplingParams
	}
	return model
}

func composeModels(providerID string, base ai.Provider, config *ModelsJsonProvider, extension *ProviderConfigInput) ([]aitypes.Model, error) {
	var models []aitypes.Model
	if base != nil {
		models = append(models, base.GetModels()...)
	}
	if config != nil {
		for i := range models {
			if config.BaseURL != "" {
				models[i].BaseUrl = config.BaseURL
			}
		}
		for _, definition := range config.Models {
			var defaults *aitypes.Model
			for i := range models {
				if models[i].Id == definition.ID {
					defaults = &models[i]
					break
				}
			}
			model, err := modelFromJSON(providerID, definition, *config, defaults)
			if err != nil {
				return nil, err
			}
			found := false
			for i := range models {
				if models[i].Id == model.Id {
					models[i] = model
					found = true
					break
				}
			}
			if !found {
				models = append(models, model)
			}
		}
		for i := range models {
			if override, ok := config.ModelOverrides[models[i].Id]; ok {
				models[i] = applyModelOverride(models[i], override)
			}
		}
	}
	if extension != nil && len(extension.Models) > 0 {
		provider := ModelsJsonProvider{BaseURL: extension.BaseURL, API: extension.API}
		if config != nil {
			provider = *config
			if extension.BaseURL != "" {
				provider.BaseURL = extension.BaseURL
			}
			if extension.API != "" {
				provider.API = extension.API
			}
		}
		var composed []aitypes.Model
		for _, definition := range extension.Models {
			var defaults *aitypes.Model
			for i := range models {
				if models[i].Id == definition.ID {
					defaults = &models[i]
					break
				}
			}
			model, err := modelFromJSON(providerID, definition, provider, defaults)
			if err != nil {
				return nil, err
			}
			composed = append(composed, model)
		}
		models = composed
	} else if extension != nil && extension.BaseURL != "" {
		for i := range models {
			models[i].BaseUrl = extension.BaseURL
		}
	}
	return models, nil
}

// ComposeModelProvider composes the built-in, models.json and custom-extension
// layers into one provider without reading credentials.
func ComposeModelProvider(providerID string, base ai.Provider, modelConfig *ModelConfig, extension *ProviderConfigInput) (ai.Provider, error) {
	config := modelConfig.providerPointer(providerID)
	if _, err := composeModels(providerID, base, config, extension); err != nil {
		return nil, err
	}

	var streams aitypes.ProviderStreams
	if extension != nil && extension.StreamSimple != nil {
		streams = &streamFnAdapter{fn: extension.StreamSimple}
	} else if base != nil {
		streams = &providerStreamsAdapter{provider: base}
	}
	if streams == nil {
		return nil, fmt.Errorf("provider %s: no stream implementation", providerID)
	}

	providerAuth := authtypes.ProviderAuth{}
	if base != nil {
		providerAuth = base.Auth()
	}
	if providerAuth.APIKey == nil && providerAuth.OAuth == nil {
		key := configuredAPIKey(config, extension)
		if key == "" {
			return nil, fmt.Errorf("provider %s: no authentication method configured", providerID)
		}
		resolved := key
		if value := ResolveConfigValue(key, nil); value != nil {
			resolved = *value
		}
		providerAuth.APIKey = &authtypes.ApiKeyAuth{
			Name: "API key",
			Resolve: func(context.Context, authtypes.ApiKeyResolveInput) (*authtypes.AuthResult, error) {
				return &authtypes.AuthResult{Auth: authtypes.ModelAuth{APIKey: &resolved}}, nil
			},
		}
	}

	baseURL := ""
	name := providerID
	var headers aitypes.ProviderHeaders
	if base != nil {
		baseURL = base.BaseURL()
		name = base.Name()
		headers = base.Headers()
	}
	if config != nil {
		if config.BaseURL != "" {
			baseURL = config.BaseURL
		}
		if config.Name != "" {
			name = config.Name
		}
	}
	if extension != nil {
		if extension.BaseURL != "" {
			baseURL = extension.BaseURL
		}
		if extension.Name != "" {
			name = extension.Name
		}
	}

	return ai.CreateProvider(ai.CreateProviderOptions{
		ID:      providerID,
		Name:    name,
		BaseURL: baseURL,
		Headers: headers,
		Auth:    providerAuth,
		Models:  nil,
		API:     streams,
	}), nil
}

func rawModelHeaders(model aitypes.Model, config *ModelsJsonProvider, extension *ProviderConfigInput) map[string]string {
	headers := map[string]string{}
	if config != nil {
		if override, ok := config.ModelOverrides[model.Id]; ok {
			for key, value := range override.Headers {
				headers[key] = value
			}
		}
		for _, definition := range config.Models {
			if definition.ID == model.Id {
				for key, value := range definition.Headers {
					headers[key] = value
				}
			}
		}
	}
	if extension != nil {
		for _, definition := range extension.Models {
			if definition.ID == model.Id {
				for key, value := range definition.Headers {
					headers[key] = value
				}
			}
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// ResolveConfiguredModelHeaders resolves configured model headers.
func ResolveConfiguredModelHeaders(model aitypes.Model, config *ModelsJsonProvider, extension *ProviderConfigInput, env map[string]string) (map[string]string, error) {
	headers := rawModelHeaders(model, config, extension)
	if len(headers) == 0 {
		return nil, nil
	}
	return ResolveHeadersOrThrow(headers, fmt.Sprintf("model %q", string(model.Provider)+"/"+model.Id), env)
}

// ResolveCompatibilityRequestConfig builds the compatibility request surface.
func ResolveCompatibilityRequestConfig(model aitypes.Model, config *ModelsJsonProvider, extension *ProviderConfigInput) CompatibilityRequestConfig {
	configured := map[string]string{}
	for key, value := range configuredHeaders(config, extension) {
		configured[key] = value
	}
	for key, value := range rawModelHeaders(model, config, extension) {
		configured[key] = value
	}
	resolved := ResolveHeaders(configured, nil)
	headers := aitypes.ProviderHeaders{}
	for key, value := range model.Headers {
		headers[key] = stringPointer(value)
	}
	for key, value := range resolved {
		headers[key] = stringPointer(value)
	}
	authHeader := false
	if config != nil && config.AuthHeader != nil {
		authHeader = *config.AuthHeader
	}
	if extension != nil && extension.AuthHeader {
		authHeader = true
	}
	if len(headers) == 0 {
		headers = nil
	}
	return CompatibilityRequestConfig{Headers: headers, AuthHeader: authHeader}
}

// ConfiguredRequestAuthStatus reports whether a configured API key is usable.
func ConfiguredRequestAuthStatus(config *ModelsJsonProvider, extension *ProviderConfigInput) *AuthStatus {
	value := configuredAPIKey(config, extension)
	if value == "" {
		return nil
	}
	if IsCommandConfigValue(value) {
		return &AuthStatus{Configured: true, Source: "models_json_command"}
	}
	names := GetConfigValueEnvVarNames(value)
	if len(names) > 0 {
		if IsConfigValueConfigured(value, nil) {
			return &AuthStatus{Configured: true, Source: "environment", Label: strings.Join(names, ", ")}
		}
		return &AuthStatus{Configured: false}
	}
	source := "models_json_key"
	if extension != nil && extension.APIKey != "" {
		source = "fallback"
	}
	return &AuthStatus{Configured: true, Source: source}
}

// ---------------------------------------------------------------------------
// Model runtime
// ---------------------------------------------------------------------------

// CreateModelRuntimeOptions configures a ModelRuntime.
type CreateModelRuntimeOptions struct {
	Credentials           authtypes.CredentialStore
	AuthPath              string
	ModelsPath            *string
	ModelsStore           ai.ModelsStore
	ModelsStorePath       string
	AllowModelNetwork     bool
	ModelRefreshTimeoutMs *int
	CatalogBaseURL        string
	Signal                context.Context
	RefreshOnCreate       *bool
	// APIKey is the injected per-provider credential callback. It is optional;
	// when absent the runtime reads the credential store.
	APIKey func(context.Context, string) (string, error)
}

// ModelRuntimeAuthOverrides are per-call auth overrides.
type ModelRuntimeAuthOverrides struct {
	authtypes.AuthOperationOptions
	APIKey             *string
	Env                map[string]string
	MinOAuthValidityMs *int
}

// CredentialSynchronizationOperation identifies a committed credential change.
type CredentialSynchronizationOperation string

// Credential synchronization operations.
const (
	CredentialSynchronizationLogin               CredentialSynchronizationOperation = "login"
	CredentialSynchronizationLogout              CredentialSynchronizationOperation = "logout"
	CredentialSynchronizationSetRuntimeAPIKey    CredentialSynchronizationOperation = "setRuntimeApiKey"
	CredentialSynchronizationRemoveRuntimeAPIKey CredentialSynchronizationOperation = "removeRuntimeApiKey"
)

// CredentialSynchronizationError reports a committed credential change whose
// local snapshot could not be synchronized.
type CredentialSynchronizationError struct {
	ProviderID string
	Operation  CredentialSynchronizationOperation
	Credential authtypes.Credential
	Cause      error
}

// Error implements error.
func (e *CredentialSynchronizationError) Error() string {
	return fmt.Sprintf("credential %s committed for %s, but local synchronization failed", e.Operation, e.ProviderID)
}

// Unwrap exposes the underlying cause.
func (e *CredentialSynchronizationError) Unwrap() error { return e.Cause }

// ModelRuntime is the configured model/auth runtime used by the coding agent.
// It owns no process-wide global registration: each runtime holds its own
// provider collection.
type ModelRuntime struct {
	mu                 sync.RWMutex
	models             ai.MutableModels
	credentials        *RuntimeCredentials
	apiKey             func(context.Context, string) (string, error)
	config             *ModelConfig
	compositionErrors  map[string]string
	extensionProviders map[string]ProviderConfigInput
	nativeProviders    map[string]ai.Provider
}

// CreateModelRuntime builds a runtime over the Pith AI provider collection.
func CreateModelRuntime(options CreateModelRuntimeOptions) (*ModelRuntime, error) {
	credentials := options.Credentials
	if credentials == nil {
		if options.AuthPath != "" {
			credentials = CreateAuthStorage(options.AuthPath)
		} else {
			credentials = auth.NewInMemoryCredentialStore()
		}
	}
	runtimeCredentials := NewRuntimeCredentials(credentials)

	modelsPath := ""
	if options.ModelsPath != nil {
		modelsPath = *options.ModelsPath
	}
	config, err := LoadModelConfig(modelsPath)
	if err != nil {
		return nil, err
	}

	modelsStore := options.ModelsStore
	if modelsStore == nil {
		if modelsPath != "" {
			storePath := options.ModelsStorePath
			if storePath == "" {
				storePath = filepathJoin(dirname(modelsPath), "models-store.json")
			}
			modelsStore = NewFileModelsStore(storePath)
		} else {
			modelsStore = NewInMemoryCodingAgentModelsStore()
		}
	}

	collection := ai.CreateModels(&ai.CreateModelsOptions{Credentials: runtimeCredentials, ModelsStore: modelsStore})
	for _, provider := range providers.BuiltinProviders() {
		collection.SetProvider(provider)
	}

	runtime := &ModelRuntime{
		models:             collection,
		credentials:        runtimeCredentials,
		apiKey:             options.APIKey,
		config:             config,
		compositionErrors:  map[string]string{},
		extensionProviders: map[string]ProviderConfigInput{},
		nativeProviders:    map[string]ai.Provider{},
	}
	return runtime, nil
}

func filepathJoin(elem ...string) string {
	return strings.Join(elem, string(os.PathSeparator))
}

func dirname(path string) string {
	index := strings.LastIndexByte(path, os.PathSeparator)
	if index < 0 {
		return "."
	}
	if index == 0 {
		return string(os.PathSeparator)
	}
	return path[:index]
}

// GetModels returns all known models.
func (r *ModelRuntime) GetModels() []aitypes.Model {
	if r == nil || r.models == nil {
		return nil
	}
	return r.models.GetModels()
}

// GetModel returns one model by provider and id.
func (r *ModelRuntime) GetModel(provider, id string) *aitypes.Model {
	if r == nil || r.models == nil {
		return nil
	}
	return r.models.GetModel(provider, id)
}

// GetProvider returns one provider.
func (r *ModelRuntime) GetProvider(id string) ai.Provider {
	if r == nil || r.models == nil {
		return nil
	}
	return r.models.GetProvider(id)
}

// GetAvailable returns models whose provider currently has configured auth.
func (r *ModelRuntime) GetAvailable(ctx context.Context, options *authtypes.AuthOperationOptions) ([]aitypes.Model, error) {
	if r == nil || r.models == nil {
		return nil, nil
	}
	return r.models.GetAvailable(ctx, "", options)
}

// GetAvailableSnapshot returns the last known available models.
func (r *ModelRuntime) GetAvailableSnapshot() []aitypes.Model {
	models, err := r.GetAvailable(context.Background(), nil)
	if err != nil {
		return nil
	}
	return models
}

// HasConfiguredAuth reports whether a provider has a usable credential.
func (r *ModelRuntime) HasConfiguredAuth(provider string) bool {
	if r == nil || r.models == nil {
		return false
	}
	check, err := r.models.CheckAuth(context.Background(), provider, nil)
	return err == nil && check != nil
}

// ResolveAPIKey returns the effective api key for a provider. It uses the
// injected callback when present, and treats an empty or failed callback as an
// error rather than silently sending an unauthenticated request.
func (r *ModelRuntime) ResolveAPIKey(ctx context.Context, provider string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r != nil && r.apiKey != nil {
		key, err := r.apiKey(ctx, provider)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(key) == "" {
			return "", fmt.Errorf("no API key available for provider %q", provider)
		}
		return key, nil
	}
	if r == nil || r.credentials == nil {
		return "", fmt.Errorf("no API key available for provider %q", provider)
	}
	credential, err := r.credentials.Read(ctx, provider, nil)
	if err != nil {
		return "", err
	}
	if apiKey, ok := credential.(*authtypes.ApiKeyCredential); ok && apiKey != nil && apiKey.Key != nil && *apiKey.Key != "" {
		return *apiKey.Key, nil
	}
	return "", fmt.Errorf("no API key available for provider %q", provider)
}

// GetAuth resolves provider-scoped auth for a model.
func (r *ModelRuntime) GetAuth(ctx context.Context, model aitypes.Model) (*authtypes.AuthResult, error) {
	if r == nil || r.models == nil {
		return nil, nil
	}
	return r.models.GetAuthForModel(ctx, model, nil)
}

// GetAuthForProvider resolves provider-scoped auth.
func (r *ModelRuntime) GetAuthForProvider(ctx context.Context, provider string) (*authtypes.AuthResult, error) {
	if r == nil || r.models == nil {
		return nil, nil
	}
	return r.models.GetAuth(ctx, provider, nil)
}

// Refresh refreshes provider catalogs.
func (r *ModelRuntime) Refresh(ctx context.Context, options *ai.ModelsRefreshOptions) (*ai.ModelsRefreshResult, error) {
	if r == nil || r.models == nil {
		return &ai.ModelsRefreshResult{}, nil
	}
	return r.models.Refresh(ctx, options)
}

// GetError returns the models.json load error, if any.
func (r *ModelRuntime) GetError() string {
	if r == nil || r.config == nil {
		return ""
	}
	return r.config.GetError()
}

// GetCompatibilityRequestConfig builds the compatibility surface for a model.
func (r *ModelRuntime) GetCompatibilityRequestConfig(model aitypes.Model) CompatibilityRequestConfig {
	var config *ModelsJsonProvider
	if r != nil && r.config != nil {
		config = r.config.providerPointer(string(model.Provider))
	}
	var extension *ProviderConfigInput
	if r != nil {
		if value, ok := r.extensionProviders[string(model.Provider)]; ok {
			extension = &value
		}
	}
	return ResolveCompatibilityRequestConfig(model, config, extension)
}

// GetProviderAuthStatus reports configured auth for a provider.
func (r *ModelRuntime) GetProviderAuthStatus(provider string) *AuthStatus {
	if r == nil {
		return nil
	}
	var config *ModelsJsonProvider
	if r.config != nil {
		config = r.config.providerPointer(provider)
	}
	var extension *ProviderConfigInput
	if value, ok := r.extensionProviders[provider]; ok {
		extension = &value
	}
	return ConfiguredRequestAuthStatus(config, extension)
}

// IsUsingOAuth reports whether a provider is authenticated via OAuth.
func (r *ModelRuntime) IsUsingOAuth(provider string) bool {
	credential, err := r.credentials.Read(context.Background(), provider, nil)
	if err != nil || credential == nil {
		return false
	}
	return credential.CredentialType() == authtypes.CredentialTypeOAuth
}

// RegisterProvider registers a named provider config.
func (r *ModelRuntime) RegisterProvider(name string, config ProviderConfigInput) error {
	if err := ValidateExtensionProvider(name, config); err != nil {
		return err
	}
	r.mu.Lock()
	r.extensionProviders[name] = config
	r.mu.Unlock()
	return r.recomposeProvider(name)
}

// RegisterNativeProvider registers a native provider instance.
func (r *ModelRuntime) RegisterNativeProvider(provider ai.Provider) {
	if provider == nil {
		return
	}
	r.mu.Lock()
	r.nativeProviders[provider.ID()] = provider
	r.mu.Unlock()
	_ = r.recomposeProvider(provider.ID())
}

// UnregisterProvider removes a named provider.
func (r *ModelRuntime) UnregisterProvider(name string) {
	r.mu.Lock()
	delete(r.extensionProviders, name)
	delete(r.nativeProviders, name)
	r.mu.Unlock()
	if r.models != nil {
		r.models.DeleteProvider(name)
	}
}

// GetRegisteredProviderConfig returns a registered provider config.
func (r *ModelRuntime) GetRegisteredProviderConfig(name string) (ProviderConfigInput, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	config, ok := r.extensionProviders[name]
	return config, ok
}

// GetRegisteredNativeProvider returns a registered native provider.
func (r *ModelRuntime) GetRegisteredNativeProvider(name string) ai.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.nativeProviders[name]
}

// GetRegisteredProviderIDs returns registered provider ids.
func (r *ModelRuntime) GetRegisteredProviderIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.extensionProviders)+len(r.nativeProviders))
	for id := range r.extensionProviders {
		ids = append(ids, id)
	}
	for id := range r.nativeProviders {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *ModelRuntime) recomposeProvider(providerID string) error {
	if r.models == nil {
		return nil
	}
	base := r.nativeProviders[providerID]
	if base == nil {
		base = r.models.GetProvider(providerID)
	}
	var extension *ProviderConfigInput
	if value, ok := r.extensionProviders[providerID]; ok {
		extension = &value
	}
	composed, err := ComposeModelProvider(providerID, base, r.config, extension)
	if err != nil {
		r.mu.Lock()
		r.compositionErrors[providerID] = err.Error()
		r.mu.Unlock()
		return err
	}
	r.mu.Lock()
	delete(r.compositionErrors, providerID)
	r.mu.Unlock()
	r.models.SetProvider(composed)
	return nil
}

// SetRuntimeAPIKey installs a non-persistent runtime API key.
func (r *ModelRuntime) SetRuntimeAPIKey(providerID, apiKey string) {
	r.credentials.SetRuntimeAPIKey(providerID, apiKey)
}

// RemoveRuntimeAPIKey removes a runtime API key.
func (r *ModelRuntime) RemoveRuntimeAPIKey(providerID string) {
	r.credentials.RemoveRuntimeAPIKey(providerID)
}

// StreamSimple streams a completion through the configured provider.
func (r *ModelRuntime) StreamSimple(ctx context.Context, model aitypes.Model, request aitypes.Context, options *ai.ModelsSimpleStreamOptions) *aitypes.AssistantMessageEventStream {
	if r == nil || r.models == nil {
		return failedStream("model runtime is not configured")
	}
	return r.models.StreamSimple(ctx, model, request, options)
}

func failedStream(message string) *aitypes.AssistantMessageEventStream {
	stream := aitypes.NewAssistantMessageEventStream()
	failed := aitypes.AssistantMessage{Role: aitypes.AssistantMessageRole, StopReason: aitypes.StopReasonError, ErrorMessage: &message}
	stream.Push(aitypes.NewErrorEvent(aitypes.StopReasonError, failed))
	stream.End(&failed)
	return stream
}

// ---------------------------------------------------------------------------
// Model registry (synchronous extension facade)
// ---------------------------------------------------------------------------

// ModelRegistry is the synchronous compatibility facade used by extensions.
type ModelRegistry struct {
	runtime *ModelRuntime
}

// NewModelRegistry wraps a runtime.
func NewModelRegistry(runtime *ModelRuntime) *ModelRegistry {
	return &ModelRegistry{runtime: runtime}
}

// GetError returns the models.json load error.
func (m *ModelRegistry) GetError() string {
	if m.runtime == nil {
		return ""
	}
	return m.runtime.GetError()
}

// GetAll returns all models.
func (m *ModelRegistry) GetAll() []aitypes.Model {
	if m.runtime == nil {
		return nil
	}
	return m.runtime.GetModels()
}

// GetAvailable returns available models.
func (m *ModelRegistry) GetAvailable() []aitypes.Model {
	if m.runtime == nil {
		return nil
	}
	return m.runtime.GetAvailableSnapshot()
}

// Find finds one model.
func (m *ModelRegistry) Find(provider, modelID string) *aitypes.Model {
	if m.runtime == nil {
		return nil
	}
	return m.runtime.GetModel(provider, modelID)
}

// HasConfiguredAuth reports configured auth for a model.
func (m *ModelRegistry) HasConfiguredAuth(model aitypes.Model) bool {
	if m.runtime == nil {
		return false
	}
	return m.runtime.HasConfiguredAuth(string(model.Provider))
}

// GetProvider returns a provider.
func (m *ModelRegistry) GetProvider(provider string) ai.Provider {
	if m.runtime == nil {
		return nil
	}
	return m.runtime.GetProvider(provider)
}

// GetProviderDisplayName returns a provider display name.
func (m *ModelRegistry) GetProviderDisplayName(provider string) string {
	if got := m.GetProvider(provider); got != nil {
		return got.Name()
	}
	return provider
}

// GetProviderAuthStatus returns provider auth status.
func (m *ModelRegistry) GetProviderAuthStatus(provider string) *AuthStatus {
	if m.runtime == nil {
		return nil
	}
	return m.runtime.GetProviderAuthStatus(provider)
}

// GetAPIKeyForProvider resolves a provider API key, returning ok=false on
// failure.
func (m *ModelRegistry) GetAPIKeyForProvider(provider string) (string, bool) {
	if m.runtime == nil {
		return "", false
	}
	key, err := m.runtime.ResolveAPIKey(context.Background(), provider)
	if err != nil || key == "" {
		return "", false
	}
	return key, true
}

// IsUsingOAuth reports whether a model's provider uses OAuth.
func (m *ModelRegistry) IsUsingOAuth(model aitypes.Model) bool {
	if m.runtime == nil {
		return false
	}
	return m.runtime.IsUsingOAuth(string(model.Provider))
}

// GetAPIKeyAndHeaders resolves request auth for a model.
func (m *ModelRegistry) GetAPIKeyAndHeaders(model aitypes.Model) ResolvedRequestAuth {
	compatibility := m.runtime.GetCompatibilityRequestConfig(model)
	if m.runtime == nil {
		return ResolvedRequestAuth{OK: true, Headers: compatibility.Headers}
	}
	key, err := m.runtime.ResolveAPIKey(context.Background(), string(model.Provider))
	if err != nil || key == "" {
		if compatibility.AuthHeader {
			return ResolvedRequestAuth{OK: false, Error: fmt.Sprintf("No API key found for %q", model.Provider)}
		}
		return ResolvedRequestAuth{OK: true, Headers: compatibility.Headers}
	}
	return ResolvedRequestAuth{OK: true, APIKey: &key, Headers: compatibility.Headers}
}

// RegisterProvider registers an extension provider.
func (m *ModelRegistry) RegisterProvider(name string, config ProviderConfigInput) error {
	if m.runtime == nil {
		return errors.New("model registry has no runtime")
	}
	return m.runtime.RegisterProvider(name, config)
}

// RegisterNativeProvider registers a native provider.
func (m *ModelRegistry) RegisterNativeProvider(provider ai.Provider) {
	if m.runtime != nil {
		m.runtime.RegisterNativeProvider(provider)
	}
}

// UnregisterProvider removes a provider.
func (m *ModelRegistry) UnregisterProvider(name string) {
	if m.runtime != nil {
		m.runtime.UnregisterProvider(name)
	}
}

// GetRegisteredProviderConfig returns a registered provider config.
func (m *ModelRegistry) GetRegisteredProviderConfig(name string) (ProviderConfigInput, bool) {
	if m.runtime == nil {
		return ProviderConfigInput{}, false
	}
	return m.runtime.GetRegisteredProviderConfig(name)
}

// GetRegisteredNativeProvider returns a registered native provider.
func (m *ModelRegistry) GetRegisteredNativeProvider(name string) ai.Provider {
	if m.runtime == nil {
		return nil
	}
	return m.runtime.GetRegisteredNativeProvider(name)
}

// GetRegisteredProviderIDs returns registered provider ids.
func (m *ModelRegistry) GetRegisteredProviderIDs() []string {
	if m.runtime == nil {
		return nil
	}
	return m.runtime.GetRegisteredProviderIDs()
}

// ---------------------------------------------------------------------------
// Glob matching for model scope patterns
// ---------------------------------------------------------------------------

func globMatch(pattern, value string) bool {
	expression := globToRegexp(pattern)
	re, err := regexp.Compile("(?i)^" + expression + "$")
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

func globToRegexp(pattern string) string {
	var builder strings.Builder
	for i := 0; i < len(pattern); i++ {
		char := pattern[i]
		switch char {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				builder.WriteString(".*")
				i++
			} else {
				builder.WriteString("[^/]*")
			}
		case '?':
			builder.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(pattern[i:], ']')
			if end == -1 {
				builder.WriteString(regexp.QuoteMeta(string(char)))
				continue
			}
			builder.WriteString(pattern[i : i+end+1])
			i += end
		default:
			builder.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	return builder.String()
}

var _ = sync.Mutex{}
