// Embedded SDK tool surface.
//
// This file ports the tool declarations of
// packages/coding-agent/src/core/tools/*.ts from Pi at revision
// f07218c4d4bbc12bef056a7058c3dd49dfe41abe and bridges them onto the accepted
// Pith agent/harness tool types. The read, write, edit and bash bodies delegate
// to the already-accepted packages/agent/harness/tools implementation through
// an ExecutionEnv, while a compact operations-backed path lets callers inject a
// custom backend. Runtime renderer metadata, JS plugin execution and bundled
// binary management (ripgrep/fd) are explicitly out of scope and are not
// represented here.
//
// Upstream: Copyright (c) 2025 Mario Zechner, MIT License.
// See the repository LICENSE for the full text.
package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	harnessenv "github.com/minifish-org/pith/packages/agent/harness/env"
	harnesstools "github.com/minifish-org/pith/packages/agent/harness/tools"
	harnesstypes "github.com/minifish-org/pith/packages/agent/harness/types"
	truncate "github.com/minifish-org/pith/packages/agent/harness/utils/truncate"
	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	aitypes "github.com/minifish-org/pith/packages/ai/types"
)

// errOperationAborted is the stable wording used when a tool observes a
// cancelled context before or after an effect.
var errOperationAborted = errors.New("Operation aborted")

// ToolDefinition is a tool the embedded SDK exposes to a model. Parameters is
// the JSON Schema document; Execute receives the raw JSON arguments after the
// registry has validated them.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Execute     func(ctx context.Context, arguments json.RawMessage) (ToolResult, error)
}

// ToolResult is the outcome of one tool execution. Content carries text and
// images, Details preserves the typed tool payload as raw JSON, and IsError is
// the tool-level error marker (distinct from a Go error, which aborts the
// call).
type ToolResult struct {
	Content []aitypes.ContentBlock
	Details json.RawMessage
	IsError bool
}

// ToolCall is one model-requested tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Tool is the agent-runtime tool view used to bridge the SDK surface onto the
// accepted Pith agent loop.
type Tool = agenttypes.AgentTool[any, any]

// ToolDef is the historical alias for a ToolDefinition.
type ToolDef = ToolDefinition

// SystemPromptContribution is the prompt metadata a tool contributes to the
// system prompt. It is data only: rendering and TUI prompts are out of scope.
type SystemPromptContribution struct {
	Snippet    string
	Guidelines []string
}

// Tool constructors used by the tool registry. The default path reuses the
// accepted harness tools bound to a local execution environment.

func newToolExecutionEnv(cwd string) harnesstypes.ExecutionEnv {
	return harnessenv.NewLocalExecutionEnv(harnessenv.LocalExecutionEnvOptions{Cwd: cwd})
}

// harnessToolToDefinition adapts a typed harness tool onto the SDK surface.
func harnessToolToDefinition[TParams any, TDetails any](
	tool harnesstypes.AgentHarnessTool[harnesstools.ExecutionToolContext, TParams, TDetails],
	env harnesstypes.ExecutionEnv,
) ToolDefinition {
	return ToolDefinition{
		Name:        tool.Tool.Name,
		Description: tool.Tool.Description,
		Parameters:  tool.Tool.Input.Schema,
		Execute: func(ctx context.Context, arguments json.RawMessage) (ToolResult, error) {
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ctx.Err(); err != nil {
				return ToolResult{}, err
			}
			params, err := tool.PrepareArguments(arguments)
			if err != nil {
				return ToolResult{}, err
			}
			result, err := tool.Execute("", params, nil, harnesstools.ExecutionToolContext{Env: env}, nil, ctx)
			if err != nil {
				return ToolResult{}, err
			}
			return toolResultFromAgent(result)
		},
	}
}

// toolResultFromAgent widens a typed harness result, preserving content,
// images and details.
func toolResultFromAgent[TDetails any](result agenttypes.AgentToolResult[TDetails]) (ToolResult, error) {
	out := ToolResult{Content: result.Content}
	if any(result.Details) != nil {
		encoded, err := json.Marshal(result.Details)
		if err != nil {
			return ToolResult{}, err
		}
		if string(encoded) != "null" {
			out.Details = encoded
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

// ReadToolInput is the parsed parameter object for the read tool.
type ReadToolInput = harnesstools.ReadToolInput

// ReadToolDetails carries the truncation metadata of a text read.
type ReadToolDetails = harnesstools.ReadToolDetails

// ReadOperations is a pluggable read backend. It mirrors the upstream
// ReadOperations interface so a caller can delegate reading to a remote
// filesystem.
type ReadOperations interface {
	// ReadFile returns the complete file contents.
	ReadFile(absolutePath string) ([]byte, error)
	// Access returns an error when the path is not readable.
	Access(absolutePath string) error
	// DetectImageMimeType returns the sniffed image MIME type, if any.
	DetectImageMimeType(absolutePath string) (string, bool, error)
}

// ReadToolOptions configures the read tool.
type ReadToolOptions struct {
	AutoResizeImages *bool
	ImageProcessor   harnesstools.ReadImageProcessor
	Operations       ReadOperations
}

// ReadToolSystemPromptContribution is the read tool prompt metadata.
var ReadToolSystemPromptContribution = SystemPromptContribution{
	Snippet:    "Read file contents",
	Guidelines: []string{"Use read to examine files instead of cat or sed."},
}

func (o *ReadToolOptions) harnessOptions() *harnesstools.ReadToolOptions {
	if o == nil {
		return nil
	}
	return &harnesstools.ReadToolOptions{
		AutoResizeImages: o.AutoResizeImages,
		ImageProcessor:   o.ImageProcessor,
	}
}

// CreateReadToolDefinition builds the read tool definition.
func CreateReadToolDefinition(cwd string, options *ReadToolOptions) ToolDefinition {
	if options != nil && options.Operations != nil {
		base := harnesstools.CreateReadTool(options.harnessOptions())
		ops := options.Operations
		return ToolDefinition{
			Name:        base.Tool.Name,
			Description: base.Tool.Description,
			Parameters:  base.Tool.Input.Schema,
			Execute: func(ctx context.Context, arguments json.RawMessage) (ToolResult, error) {
				if ctx == nil {
					ctx = context.Background()
				}
				params, err := base.PrepareArguments(arguments)
				if err != nil {
					return ToolResult{}, err
				}
				return executeReadWithOperations(cwd, ops, params, ctx)
			},
		}
	}
	return harnessToolToDefinition(harnesstools.CreateReadTool(options.harnessOptions()), newToolExecutionEnv(cwd))
}

// CreateReadTool builds the read tool as an agent-runtime tool.
func CreateReadTool(cwd string, options *ReadToolOptions) Tool {
	return WrapToolDefinition(CreateReadToolDefinition(cwd, options))
}

func executeReadWithOperations(cwd string, ops ReadOperations, params harnesstools.ReadToolInput, ctx context.Context) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	absolutePath := ResolveReadPath(params.Path, cwd)
	if err := ops.Access(absolutePath); err != nil {
		return ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	mimeType, isImage, err := ops.DetectImageMimeType(absolutePath)
	if err != nil {
		return ToolResult{}, err
	}
	if isImage {
		content, err := ops.ReadFile(absolutePath)
		if err != nil {
			return ToolResult{}, err
		}
		return ToolResult{Content: []aitypes.ContentBlock{
			aitypes.TextBlock(fmt.Sprintf("Read image file [%s]", mimeType)),
			aitypes.ImageBlock(harnesstools.EncodeBase64(content), mimeType),
		}}, nil
	}
	content, err := ops.ReadFile(absolutePath)
	if err != nil {
		return ToolResult{}, err
	}
	return executeReadTextWithOperations(params, content)
}

func executeReadTextWithOperations(params harnesstools.ReadToolInput, content []byte) (ToolResult, error) {
	text := decodeTextContent(content)
	allLines := strings.Split(text, "\n")
	totalFileLines := len(allLines)

	startLine := 0
	if params.Offset != nil && *params.Offset > 0 {
		startLine = *params.Offset - 1
	}
	startLineDisplay := startLine + 1
	if startLine >= len(allLines) {
		return ToolResult{}, fmt.Errorf("Offset %d is beyond end of file (%d lines total)", valueOrZero(params.Offset), len(allLines))
	}

	var selectedContent string
	var userLimitedLines *int
	if params.Limit != nil {
		endLine := startLine + *params.Limit
		if endLine > len(allLines) {
			endLine = len(allLines)
		}
		sliceEnd := endLine
		if sliceEnd < startLine {
			sliceEnd = startLine
		}
		selectedContent = strings.Join(allLines[startLine:sliceEnd], "\n")
		limited := endLine - startLine
		userLimitedLines = &limited
	} else {
		selectedContent = strings.Join(allLines[startLine:], "\n")
	}

	result := truncate.TruncateHead(selectedContent, truncate.TruncationOptions{})
	var outputText string
	var details *harnesstools.ReadToolDetails
	switch {
	case result.FirstLineExceedsLimit:
		firstLineSize := truncate.FormatSize(len(allLines[startLine]))
		outputText = fmt.Sprintf(
			"[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
			startLineDisplay,
			firstLineSize,
			truncate.FormatSize(truncate.DefaultMaxBytes),
			startLineDisplay,
			params.Path,
			truncate.DefaultMaxBytes,
		)
		details = &harnesstools.ReadToolDetails{Truncation: &result}
	case result.Truncated:
		endLineDisplay := startLineDisplay + result.OutputLines - 1
		nextOffset := endLineDisplay + 1
		outputText = result.Content
		if result.TruncatedBy != nil && *result.TruncatedBy == "lines" {
			outputText += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", startLineDisplay, endLineDisplay, totalFileLines, nextOffset)
		} else {
			outputText += fmt.Sprintf(
				"\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]",
				startLineDisplay,
				endLineDisplay,
				totalFileLines,
				truncate.FormatSize(truncate.DefaultMaxBytes),
				nextOffset,
			)
		}
		details = &harnesstools.ReadToolDetails{Truncation: &result}
	case userLimitedLines != nil && startLine+*userLimitedLines < len(allLines):
		remaining := len(allLines) - (startLine + *userLimitedLines)
		nextOffset := startLine + *userLimitedLines + 1
		outputText = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]", result.Content, remaining, nextOffset)
	default:
		outputText = result.Content
	}

	return toolResultFromAgent(agenttypes.AgentToolResult[*harnesstools.ReadToolDetails]{
		Content: []aitypes.ContentBlock{aitypes.TextBlock(outputText)},
		Details: details,
	})
}

func valueOrZero(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// decodeTextContent mirrors the upstream non-fatal UTF-8 decoding.
func decodeTextContent(content []byte) string {
	return strings.ToValidUTF8(string(content), "\uFFFD")
}

// ---------------------------------------------------------------------------
// write
// ---------------------------------------------------------------------------

// WriteToolInput is the parsed parameter object for the write tool.
type WriteToolInput = harnesstools.WriteToolInput

// WriteOperations is a pluggable write backend.
type WriteOperations interface {
	// WriteFile writes content to an absolute path.
	WriteFile(absolutePath string, content []byte) error
	// Mkdir creates a directory and its parents.
	Mkdir(dir string) error
	// CanonicalPath resolves the canonical mutation key for a path.
	CanonicalPath(absolutePath string) (string, error)
}

// WriteToolOptions configures the write tool.
type WriteToolOptions struct {
	Operations WriteOperations
}

// WriteToolSystemPromptContribution is the write tool prompt metadata.
var WriteToolSystemPromptContribution = SystemPromptContribution{
	Snippet:    "Create or overwrite files",
	Guidelines: []string{"Use write only for new files or complete rewrites."},
}

// CreateWriteToolDefinition builds the write tool definition.
func CreateWriteToolDefinition(cwd string, options *WriteToolOptions) ToolDefinition {
	if options != nil && options.Operations != nil {
		base := harnesstools.CreateWriteTool()
		ops := options.Operations
		return ToolDefinition{
			Name:        base.Tool.Name,
			Description: base.Tool.Description,
			Parameters:  base.Tool.Input.Schema,
			Execute: func(ctx context.Context, arguments json.RawMessage) (ToolResult, error) {
				if ctx == nil {
					ctx = context.Background()
				}
				params, err := base.PrepareArguments(arguments)
				if err != nil {
					return ToolResult{}, err
				}
				return executeWriteWithOperations(cwd, ops, params, ctx)
			},
		}
	}
	return harnessToolToDefinition(harnesstools.CreateWriteTool(), newToolExecutionEnv(cwd))
}

// CreateWriteTool builds the write tool as an agent-runtime tool.
func CreateWriteTool(cwd string, options *WriteToolOptions) Tool {
	return WrapToolDefinition(CreateWriteToolDefinition(cwd, options))
}

func executeWriteWithOperations(cwd string, ops WriteOperations, params harnesstools.WriteToolInput, ctx context.Context) (ToolResult, error) {
	absolutePath := ResolveToCwd(params.Path, cwd)
	canonical := absolutePath
	if resolved, err := ops.CanonicalPath(absolutePath); err == nil {
		canonical = resolved
	}
	return withPathMutationQueue(canonical, func() (ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		if err := ops.Mkdir(filepath.Dir(absolutePath)); err != nil {
			return ToolResult{}, err
		}
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		if err := ops.WriteFile(absolutePath, []byte(params.Content)); err != nil {
			return ToolResult{}, err
		}
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		return ToolResult{Content: []aitypes.ContentBlock{aitypes.TextBlock(fmt.Sprintf("Successfully wrote to %s", params.Path))}}, nil
	})
}

// ---------------------------------------------------------------------------
// edit
// ---------------------------------------------------------------------------

// Edit is one exact-text replacement request.
type Edit = harnesstools.Edit

// EditToolInput is the parsed parameter object for the edit tool.
type EditToolInput = harnesstools.EditToolInput

// EditToolDetails carries the diff payload of a successful edit.
type EditToolDetails = harnesstools.EditToolDetails

// EditOperations is a pluggable edit backend.
type EditOperations interface {
	ReadFile(absolutePath string) ([]byte, error)
	WriteFile(absolutePath string, content []byte) error
	Stat(absolutePath string) (EditFileInfo, error)
	CanonicalPath(absolutePath string) (string, error)
}

// EditFileInfo is the edit backend's file metadata.
type EditFileInfo struct {
	IsFile    bool
	IsSymlink bool
}

// EditToolOptions configures the edit tool.
type EditToolOptions struct {
	Operations EditOperations
}

// EditToolSystemPromptContribution is the edit tool prompt metadata.
var EditToolSystemPromptContribution = SystemPromptContribution{
	Snippet: "Edit files with exact text replacement",
	Guidelines: []string{
		"Every edits[].oldText must match a unique, non-overlapping region of the original file.",
	},
}

// CreateEditToolDefinition builds the edit tool definition.
func CreateEditToolDefinition(cwd string, options *EditToolOptions) ToolDefinition {
	if options != nil && options.Operations != nil {
		base := harnesstools.CreateEditTool()
		ops := options.Operations
		return ToolDefinition{
			Name:        base.Tool.Name,
			Description: base.Tool.Description,
			Parameters:  base.Tool.Input.Schema,
			Execute: func(ctx context.Context, arguments json.RawMessage) (ToolResult, error) {
				if ctx == nil {
					ctx = context.Background()
				}
				params, err := base.PrepareArguments(arguments)
				if err != nil {
					return ToolResult{}, err
				}
				return executeEditWithOperations(cwd, ops, params, ctx)
			},
		}
	}
	return harnessToolToDefinition(harnesstools.CreateEditTool(), newToolExecutionEnv(cwd))
}

// CreateEditTool builds the edit tool as an agent-runtime tool.
func CreateEditTool(cwd string, options *EditToolOptions) Tool {
	return WrapToolDefinition(CreateEditToolDefinition(cwd, options))
}

func executeEditWithOperations(cwd string, ops EditOperations, params harnesstools.EditToolInput, ctx context.Context) (ToolResult, error) {
	path, edits, err := harnesstools.ValidateEditInput(params)
	if err != nil {
		return ToolResult{}, err
	}
	absolutePath := ResolveToCwd(path, cwd)
	canonical := absolutePath
	if resolved, err := ops.CanonicalPath(absolutePath); err == nil {
		canonical = resolved
	}
	return withPathMutationQueue(canonical, func() (ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		info, err := ops.Stat(absolutePath)
		if err != nil {
			return ToolResult{}, fmt.Errorf("Could not edit file: %s. %v", path, err)
		}
		if !info.IsFile && !info.IsSymlink {
			return ToolResult{}, fmt.Errorf("Could not edit file: %s. Path is not a file.", path)
		}
		raw, err := ops.ReadFile(absolutePath)
		if err != nil {
			return ToolResult{}, fmt.Errorf("Could not edit file: %s. %v", path, err)
		}
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		bom, content := harnesstools.StripBom(string(raw))
		originalEnding := harnesstools.DetectLineEnding(content)
		normalizedContent := harnesstools.NormalizeToLF(content)
		applied, err := harnesstools.ApplyEditsToNormalizedContent(normalizedContent, edits, path)
		if err != nil {
			return ToolResult{}, err
		}
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		finalContent := bom + harnesstools.RestoreLineEndings(applied.NewContent, originalEnding)
		if err := ops.WriteFile(absolutePath, []byte(finalContent)); err != nil {
			return ToolResult{}, fmt.Errorf("Could not edit file: %s. %v", path, err)
		}
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		diff, firstChangedLine := harnesstools.GenerateDiffString(applied.BaseContent, applied.NewContent)
		return toolResultFromAgent(agenttypes.AgentToolResult[*harnesstools.EditToolDetails]{
			Content: []aitypes.ContentBlock{aitypes.TextBlock(fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(edits), path))},
			Details: &harnesstools.EditToolDetails{
				Diff:             diff,
				Patch:            harnesstools.GenerateUnifiedPatch(path, applied.BaseContent, applied.NewContent),
				FirstChangedLine: firstChangedLine,
			},
		})
	})
}

// ---------------------------------------------------------------------------
// edit-diff
// ---------------------------------------------------------------------------

// DetectLineEnding returns the dominant line ending of content.
func DetectLineEnding(content string) string { return harnesstools.DetectLineEnding(content) }

// NormalizeToLF converts CRLF and CR line endings to LF.
func NormalizeToLF(text string) string { return harnesstools.NormalizeToLF(text) }

// RestoreLineEndings restores the requested line ending.
func RestoreLineEndings(text string, ending string) string {
	return harnesstools.RestoreLineEndings(text, ending)
}

// NormalizeForFuzzyMatch normalizes text for fuzzy matching.
func NormalizeForFuzzyMatch(text string) string { return harnesstools.NormalizeForFuzzyMatch(text) }

// ApplyReplacementsPreservingUnchangedLines is re-exported from the harness.
func ApplyReplacementsPreservingUnchangedLines(originalContent string, baseContent string, replacements []harnesstools.TextReplacement) (string, error) {
	return harnesstools.ApplyReplacementsPreservingUnchangedLines(originalContent, baseContent, replacements)
}

// FuzzyMatchResult is the outcome of an exact-then-fuzzy text search.
type FuzzyMatchResult = harnesstools.FuzzyMatchResult

// AppliedEditsResult is the base and rewritten content produced by applying a
// set of edits.
type AppliedEditsResult = harnesstools.AppliedEditsResult

// FuzzyFindText finds oldText in content, trying an exact match first.
func FuzzyFindText(content string, oldText string) FuzzyMatchResult {
	return harnesstools.FuzzyFindText(content, oldText)
}

// ApplyEditsToNormalizedContent applies edits against normalized content.
func ApplyEditsToNormalizedContent(normalizedContent string, edits []Edit, path string) (AppliedEditsResult, error) {
	return harnesstools.ApplyEditsToNormalizedContent(normalizedContent, edits, path)
}

// GenerateUnifiedPatch builds a unified patch for the content change.
func GenerateUnifiedPatch(path string, oldContent string, newContent string, contextLines ...int) string {
	return harnesstools.GenerateUnifiedPatch(path, oldContent, newContent, contextLines...)
}

// GenerateDiffString builds the display diff and first changed line.
func GenerateDiffString(oldContent string, newContent string, contextLines ...int) (string, *int) {
	return harnesstools.GenerateDiffString(oldContent, newContent, contextLines...)
}

// EditDiffResult is the diff preview for one or more edits.
type EditDiffResult struct {
	Diff             string `json:"diff"`
	FirstChangedLine *int   `json:"firstChangedLine,omitempty"`
}

// EditDiffError is the failure preview for one or more edits.
type EditDiffError struct {
	Error string `json:"error"`
}

// ComputeEditsDiff computes the diff for edits without applying them.
func ComputeEditsDiff(path string, edits []Edit, cwd string) (EditDiffResult, *EditDiffError) {
	absolutePath := ResolveToCwd(path, cwd)
	raw, err := os.ReadFile(absolutePath)
	if err != nil {
		return EditDiffResult{}, &EditDiffError{Error: fmt.Sprintf("Could not edit file: %s. %v.", path, err)}
	}
	_, content := harnesstools.StripBom(string(raw))
	normalized := harnesstools.NormalizeToLF(content)
	applied, applyErr := harnesstools.ApplyEditsToNormalizedContent(normalized, edits, path)
	if applyErr != nil {
		return EditDiffResult{}, &EditDiffError{Error: applyErr.Error()}
	}
	diff, firstChangedLine := harnesstools.GenerateDiffString(applied.BaseContent, applied.NewContent)
	return EditDiffResult{Diff: diff, FirstChangedLine: firstChangedLine}, nil
}

// ComputeEditDiff is the single-edit convenience wrapper around ComputeEditsDiff.
func ComputeEditDiff(path string, oldText string, newText string, cwd string) (EditDiffResult, *EditDiffError) {
	return ComputeEditsDiff(path, []Edit{{OldText: oldText, NewText: newText}}, cwd)
}

// ---------------------------------------------------------------------------
// bash / shell
// ---------------------------------------------------------------------------

// BashToolInput is the parsed parameter object for the shell tools.
type BashToolInput = harnesstools.BashToolInput

// BashToolDetails carries the truncation metadata and spill path of a shell run.
type BashToolDetails = harnesstools.BashToolDetails

// BashExecOptions are the per-call options passed to a BashOperations backend.
type BashExecOptions struct {
	OnData  func(data []byte)
	Env     map[string]string
	Timeout *float64
}

// BashExecResult is the exit outcome of one command.
type BashExecResult struct {
	ExitCode int
	Signal   string
}

// BashOperations is a pluggable command execution backend. It mirrors the
// upstream BashOperations interface.
type BashOperations interface {
	Exec(ctx context.Context, command string, cwd string, options BashExecOptions) (BashExecResult, error)
}

// BashSpawnContext is the command, working directory and environment passed to
// a spawn hook.
type BashSpawnContext struct {
	Command string
	Cwd     string
	Env     map[string]string
}

// BashSpawnHook may rewrite the command, working directory or environment
// before a shell tool spawns a process.
type BashSpawnHook func(context BashSpawnContext) BashSpawnContext

// BashToolOptions configures the shell tools.
type BashToolOptions struct {
	Operations               BashOperations
	CommandPrefix            string
	ShellPath                *string
	ExposeSessionEnvironment *bool
	SpawnHook                BashSpawnHook
	Prepare                  func(execution *BashExecutionContext, ctx context.Context) error
}

// BashExecutionContext is the mutable command plan passed to Prepare.
type BashExecutionContext struct {
	Command    string
	Cwd        string
	Env        map[string]string
	InheritEnv bool
}

// BashRenderState is the mutable render state retained for parity. It is data
// only; the SDK renders no widgets.
type BashRenderState struct {
	StartedAt *time.Time
	EndedAt   *time.Time
}

// ShellToolConfig names one shell tool.
type ShellToolConfig struct {
	Name             string
	Label            string
	ShellName        string
	Prompt           string
	PromptSnippet    string
	PromptGuidelines []string
	TempFilePrefix   string
}

// BashToolSystemPromptContribution is the bash prompt metadata.
var BashToolSystemPromptContribution = SystemPromptContribution{
	Snippet:    "Execute bash commands (ls, grep, find, etc.)",
	Guidelines: []string{"You can inspect PI_* environment variables for current model and session details."},
}

type localShellOperations struct{ shellName string }

// CreateLocalShellOperations builds a local command backend. The process runs
// through the accepted harness execution environment so cancellation reaches
// the whole process group.
func CreateLocalShellOperations(shellName string) BashOperations {
	return &localShellOperations{shellName: shellName}
}

// CreateLocalBashOperations builds the local bash backend.
func CreateLocalBashOperations(options *BashToolOptions) BashOperations {
	return CreateLocalShellOperations("bash")
}

func (o *localShellOperations) Exec(ctx context.Context, command string, cwd string, options BashExecOptions) (BashExecResult, error) {
	env := harnessenv.NewLocalExecutionEnv(harnessenv.LocalExecutionEnvOptions{Cwd: cwd})
	capture := harnesstypes.ShellOutputCaptureOptions{}
	result := env.Exec(command, &harnesstypes.ShellExecOptions{
		Cwd:      &cwd,
		Env:      options.Env,
		Timeout:  options.Timeout,
		Capture:  &capture,
		OnUpdate: nil,
	}, ctx)
	if !result.OK {
		if result.Error.Code == harnesstypes.ExecutionErrorAborted {
			return BashExecResult{}, errOperationAborted
		}
		return BashExecResult{}, errors.New(result.Error.Message)
	}
	return BashExecResult{ExitCode: result.Value.ExitCode}, nil
}

// CreateShellToolDefinition builds a shell tool definition from a config.
func CreateShellToolDefinition(cwd string, config ShellToolConfig, options *BashToolOptions) ToolDefinition {
	return createShellDefinition(cwd, config, options)
}

// CreateBashToolDefinition builds the bash tool definition.
func CreateBashToolDefinition(cwd string, options *BashToolOptions) ToolDefinition {
	return createShellDefinition(cwd, bashToolConfig, options)
}

// CreateBashTool builds the bash tool as an agent-runtime tool.
func CreateBashTool(cwd string, options *BashToolOptions) Tool {
	return WrapToolDefinition(CreateBashToolDefinition(cwd, options))
}

var bashToolConfig = ShellToolConfig{
	Name:           "bash",
	Label:          "bash",
	ShellName:      "bash",
	Prompt:         "$",
	PromptSnippet:  BashToolSystemPromptContribution.Snippet,
	TempFilePrefix: "pi-bash",
}

func createShellDefinition(cwd string, config ShellToolConfig, options *BashToolOptions) ToolDefinition {
	if options == nil || options.Operations == nil {
		harnessOptions := &harnesstools.BashToolOptions{}
		if options != nil {
			harnessOptions.CommandPrefix = options.CommandPrefix
			if options.Prepare != nil {
				prepare := options.Prepare
				harnessOptions.Prepare = func(execution *harnesstools.BashExecution, _ harnesstools.ExecutionToolContext, ctx context.Context) error {
					plan := &BashExecutionContext{
						Command:    execution.Command,
						Cwd:        execution.Cwd,
						Env:        execution.Env,
						InheritEnv: execution.InheritEnv,
					}
					if err := prepare(plan, ctx); err != nil {
						return err
					}
					execution.Command = plan.Command
					execution.Cwd = plan.Cwd
					execution.Env = plan.Env
					execution.InheritEnv = plan.InheritEnv
					return nil
				}
			}
		}
		tool := harnesstools.CreateBashTool(harnessOptions)
		definition := harnessToolToDefinition(tool, newToolExecutionEnv(cwd))
		definition.Name = config.Name
		return definition
	}

	base := harnesstools.CreateBashTool(nil)
	ops := options.Operations
	commandPrefix := options.CommandPrefix
	spawnHook := options.SpawnHook
	prepare := options.Prepare
	return ToolDefinition{
		Name:        config.Name,
		Description: base.Tool.Description,
		Parameters:  base.Tool.Input.Schema,
		Execute: func(ctx context.Context, arguments json.RawMessage) (ToolResult, error) {
			if ctx == nil {
				ctx = context.Background()
			}
			params, err := base.PrepareArguments(arguments)
			if err != nil {
				return ToolResult{}, err
			}
			return executeBashWithOperations(cwd, config, ops, commandPrefix, spawnHook, prepare, params, ctx)
		},
	}
}

func executeBashWithOperations(
	cwd string,
	config ShellToolConfig,
	ops BashOperations,
	commandPrefix string,
	spawnHook BashSpawnHook,
	prepare func(execution *BashExecutionContext, ctx context.Context) error,
	params harnesstools.BashToolInput,
	ctx context.Context,
) (ToolResult, error) {
	command := params.Command
	if commandPrefix != "" {
		command = commandPrefix + "\n" + command
	}
	plan := &BashExecutionContext{Command: command, Cwd: cwd, Env: map[string]string{}, InheritEnv: true}
	if prepare != nil {
		if err := prepare(plan, ctx); err != nil {
			return ToolResult{}, err
		}
	}
	spawnContext := BashSpawnContext{Command: plan.Command, Cwd: plan.Cwd, Env: plan.Env}
	if spawnHook != nil {
		spawnContext = spawnHook(spawnContext)
	}
	prefix := config.TempFilePrefix
	if prefix == "" {
		prefix = "pi-output"
	}
	accumulator := NewOutputAccumulator(&OutputAccumulatorOptions{TempFilePrefix: &prefix})
	defer accumulator.Close()

	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	execResult, err := ops.Exec(ctx, spawnContext.Command, spawnContext.Cwd, BashExecOptions{
		OnData:  func(data []byte) { accumulator.Append(data) },
		Env:     spawnContext.Env,
		Timeout: params.Timeout,
	})
	accumulator.Finish()
	snapshot := accumulator.Snapshot(&OutputSnapshotOptions{PersistIfTruncated: true})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ToolResult{}, errOperationAborted
		}
		text := snapshot.Content
		if text != "" {
			return ToolResult{}, fmt.Errorf("%s\n\n%v", text, err)
		}
		return ToolResult{}, err
	}
	outputText := snapshot.Content
	if outputText == "" {
		outputText = "(no output)"
	}
	var details *harnesstools.BashToolDetails
	if snapshot.Truncation.Truncated {
		converted := harnesstypes.ShellOutputTruncationFrom(snapshot.Truncation)
		details = &harnesstools.BashToolDetails{Truncation: &converted, FullOutputPath: snapshot.FullOutputPath}
	}
	if execResult.ExitCode != 0 {
		if outputText != "" {
			return ToolResult{}, fmt.Errorf("%s\n\nCommand exited with code %d", outputText, execResult.ExitCode)
		}
		return ToolResult{}, fmt.Errorf("Command exited with code %d", execResult.ExitCode)
	}
	return toolResultFromAgent(agenttypes.AgentToolResult[*harnesstools.BashToolDetails]{
		Content: []aitypes.ContentBlock{aitypes.TextBlock(outputText)},
		Details: details,
	})
}

// ---------------------------------------------------------------------------
// powershell (adapted shell configuration)
// ---------------------------------------------------------------------------

const powerShellUTF8OutputPrefix = "try { [Console]::OutputEncoding=[System.Text.Encoding]::UTF8 } catch {}\n"

// PowerShellOperations is the PowerShell command backend.
type PowerShellOperations = BashOperations

// PowerShellSpawnContext is the PowerShell spawn context.
type PowerShellSpawnContext = BashSpawnContext

// PowerShellSpawnHook is the PowerShell spawn hook.
type PowerShellSpawnHook = BashSpawnHook

// PowerShellToolDetails is the PowerShell result details.
type PowerShellToolDetails = BashToolDetails

// PowerShellToolInput is the PowerShell parameter object.
type PowerShellToolInput = BashToolInput

// PowerShellToolOptions configures the PowerShell tool.
type PowerShellToolOptions struct {
	Operations               BashOperations
	ExposeSessionEnvironment *bool
	SpawnHook                BashSpawnHook
}

// PowerShellToolSystemPromptContribution is the PowerShell prompt metadata.
var PowerShellToolSystemPromptContribution = SystemPromptContribution{
	Snippet:    "Execute PowerShell commands",
	Guidelines: []string{"You can inspect PI_* environment variables for current model and session details."},
}

// CreateLocalPowerShellOperations builds the local PowerShell backend.
func CreateLocalPowerShellOperations() PowerShellOperations {
	return CreateLocalShellOperations("PowerShell")
}

var powerShellToolConfig = ShellToolConfig{
	Name:           "powershell",
	Label:          "powershell",
	ShellName:      "PowerShell",
	Prompt:         "PS>",
	PromptSnippet:  PowerShellToolSystemPromptContribution.Snippet,
	TempFilePrefix: "pi-powershell",
}

// CreatePowerShellToolDefinition builds the PowerShell tool definition.
func CreatePowerShellToolDefinition(cwd string, options *PowerShellToolOptions) ToolDefinition {
	bashOptions := &BashToolOptions{CommandPrefix: powerShellUTF8OutputPrefix}
	if options != nil {
		bashOptions.Operations = options.Operations
		bashOptions.SpawnHook = options.SpawnHook
		bashOptions.ExposeSessionEnvironment = options.ExposeSessionEnvironment
	}
	return createShellDefinition(cwd, powerShellToolConfig, bashOptions)
}

// CreatePowerShellTool builds the PowerShell tool as an agent-runtime tool.
func CreatePowerShellTool(cwd string, options *PowerShellToolOptions) Tool {
	return WrapToolDefinition(CreatePowerShellToolDefinition(cwd, options))
}

// ---------------------------------------------------------------------------
// output accumulator
// ---------------------------------------------------------------------------

// OutputAccumulatorOptions configures an OutputAccumulator.
type OutputAccumulatorOptions struct {
	MaxLines       *int
	MaxBytes       *int
	TempFilePrefix *string
}

// OutputSnapshotOptions configures one snapshot.
type OutputSnapshotOptions struct {
	PersistIfTruncated bool
}

// OutputSnapshot is a bounded output view with truncation metadata.
type OutputSnapshot struct {
	Content        string
	Truncation     truncate.TruncationResult
	FullOutputPath *string
}

// OutputAccumulator incrementally tracks streaming output with bounded memory
// and spills the complete stream to a temp file when it is truncated.
type OutputAccumulator struct {
	mu        sync.Mutex
	maxLines  int
	maxBytes  int
	prefix    string
	tail      []byte
	rawCount  int
	lineCount int
	finished  bool
	tempPath  string
	tempFile  *os.File
}

// NewOutputAccumulator builds an accumulator with the supplied limits.
func NewOutputAccumulator(options *OutputAccumulatorOptions) *OutputAccumulator {
	maxLines := truncate.DefaultMaxLines
	maxBytes := truncate.DefaultMaxBytes
	prefix := "pi-output"
	if options != nil {
		if options.MaxLines != nil {
			maxLines = *options.MaxLines
		}
		if options.MaxBytes != nil {
			maxBytes = *options.MaxBytes
		}
		if options.TempFilePrefix != nil && *options.TempFilePrefix != "" {
			prefix = *options.TempFilePrefix
		}
	}
	return &OutputAccumulator{maxLines: maxLines, maxBytes: maxBytes, prefix: prefix}
}

// Append adds one output chunk.
func (a *OutputAccumulator) Append(data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return
	}
	a.rawCount += len(data)
	for _, b := range data {
		if b == '\n' {
			a.lineCount++
		}
	}
	if len(a.tail) < a.maxBytes*2 {
		a.tail = append(a.tail, data...)
	} else {
		// Keep a bounded tail; spill the earlier bytes to the temp file.
		a.ensureTempFileLocked()
		if a.tempFile != nil {
			_, _ = a.tempFile.Write(data)
		}
		a.tail = append(a.tail, data...)
		if len(a.tail) > a.maxBytes*2 {
			a.tail = append([]byte(nil), a.tail[len(a.tail)-a.maxBytes*2:]...)
		}
	}
	if a.tempFile == nil && (a.rawCount > a.maxBytes || a.lineCount > a.maxLines) {
		a.ensureTempFileLocked()
	}
}

// Finish finalizes the accumulator.
func (a *OutputAccumulator) Finish() {
	a.mu.Lock()
	a.finished = true
	a.mu.Unlock()
}

// Snapshot returns the current bounded view. It never mutates content.
func (a *OutputAccumulator) Snapshot(options *OutputSnapshotOptions) OutputSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	persist := options != nil && options.PersistIfTruncated
	content := string(a.tail)
	truncation := truncate.TruncateTail(content, truncate.TruncationOptions{
		MaxLines: intPointer(a.maxLines),
		MaxBytes: intPointer(a.maxBytes),
	})
	truncated := a.lineCount > a.maxLines || a.rawCount > a.maxBytes
	if truncated {
		truncation.Truncated = true
		truncation.TotalLines = a.lineCount
		truncation.TotalBytes = a.rawCount
		if truncation.TruncatedBy == nil {
			if a.rawCount > a.maxBytes {
				by := "bytes"
				truncation.TruncatedBy = &by
			} else {
				by := "lines"
				truncation.TruncatedBy = &by
			}
		}
	}
	snapshot := OutputSnapshot{Content: truncation.Content, Truncation: truncation}
	if persist && truncated {
		a.ensureTempFileLocked()
	}
	if a.tempPath != "" {
		path := a.tempPath
		snapshot.FullOutputPath = &path
	}
	return snapshot
}

// Close releases the spill file.
func (a *OutputAccumulator) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tempFile != nil {
		err := a.tempFile.Close()
		a.tempFile = nil
		return err
	}
	return nil
}

func (a *OutputAccumulator) ensureTempFileLocked() {
	if a.tempFile != nil {
		return
	}
	file, err := os.CreateTemp("", a.prefix+"-*.log")
	if err != nil {
		return
	}
	a.tempFile = file
	a.tempPath = file.Name()
}

func intPointer(value int) *int { return &value }

// ---------------------------------------------------------------------------
// truncate re-exports
// ---------------------------------------------------------------------------

// Truncation limits and the truncation result/option types are re-exported
// from the accepted harness truncation package so the SDK has one owner.
const (
	// DefaultMaxLines is the default line limit.
	DefaultMaxLines = truncate.DefaultMaxLines
	// DefaultMaxBytes is the default byte limit.
	DefaultMaxBytes = truncate.DefaultMaxBytes
	// GrepMaxLineLength is the maximum character count of one grep match line.
	GrepMaxLineLength = truncate.GrepMaxLineLength
)

// TruncationResult describes one truncation decision.
type TruncationResult = truncate.TruncationResult

// TruncationOptions overrides the default truncation limits.
type TruncationOptions = truncate.TruncationOptions

// FormatSize renders bytes as a human-readable size.
func FormatSize(bytes int) string { return truncate.FormatSize(bytes) }

// TruncateHead keeps the first N lines/bytes.
func TruncateHead(content string, options TruncationOptions) TruncationResult {
	return truncate.TruncateHead(content, options)
}

// TruncateTail keeps the last N lines/bytes.
func TruncateTail(content string, options TruncationOptions) TruncationResult {
	return truncate.TruncateTail(content, options)
}

// TruncateLine truncates a single line to maxChars.
func TruncateLine(line string, maxChars int) (string, bool) {
	return truncate.TruncateLine(line, maxChars)
}

// ---------------------------------------------------------------------------
// agent-runtime bridge
// ---------------------------------------------------------------------------

// WrapToolDefinition adapts an SDK ToolDefinition onto the agent-runtime tool
// type so the accepted agent loop can execute it.
func WrapToolDefinition(definition ToolDefinition) Tool {
	return agenttypes.AgentTool[any, any]{
		Tool: aitypes.Tool{
			Name:        definition.Name,
			Description: definition.Description,
			Input:       aitypes.JSONSchemaToolInput(definition.Parameters),
		},
		Label:            definition.Name,
		PrepareArguments: func(args any) (any, error) { return prepareRawArguments(args) },
		Execute: func(toolCallID string, params any, signal <-chan struct{}, _ agenttypes.AgentToolUpdateCallback[any]) (agenttypes.AgentToolResult[any], error) {
			ctx := contextFromSignal(signal)
			raw, err := prepareRawArguments(params)
			if err != nil {
				return agenttypes.AgentToolResult[any]{}, err
			}
			result, err := definition.Execute(ctx, raw)
			if err != nil {
				return agenttypes.AgentToolResult[any]{}, err
			}
			converted := agenttypes.AgentToolResult[any]{
				Content: result.Content,
				Details: decodeDetails(result.Details),
			}
			if result.IsError {
				return converted, errors.New(toolResultText(result))
			}
			return converted, nil
		},
	}
}

// WrapToolDefinitions adapts a slice of SDK tool definitions.
func WrapToolDefinitions(definitions []ToolDefinition) []Tool {
	out := make([]Tool, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, WrapToolDefinition(definition))
	}
	return out
}

// CreateToolDefinitionFromAgentTool synthesizes an SDK definition from an
// agent-runtime tool. It is definition-first so an application can register a
// plain AgentTool without supplying prompt metadata.
func CreateToolDefinitionFromAgentTool(tool Tool) ToolDefinition {
	return ToolDefinition{
		Name:        tool.Tool.Name,
		Description: tool.Tool.Description,
		Parameters:  tool.Tool.Input.Schema,
		Execute: func(ctx context.Context, arguments json.RawMessage) (ToolResult, error) {
			params, err := tool.PrepareArguments(arguments)
			if err != nil {
				return ToolResult{}, err
			}
			result, err := tool.Execute(tool.Tool.Name, params, contextDone(ctx), nil)
			if err != nil {
				return ToolResult{IsError: true, Content: []aitypes.ContentBlock{aitypes.TextBlock(err.Error())}}, nil
			}
			encoded, encodeErr := json.Marshal(result.Details)
			if encodeErr != nil {
				encoded = nil
			}
			if string(encoded) == "null" {
				encoded = nil
			}
			return ToolResult{Content: result.Content, Details: encoded}, nil
		},
	}
}

func prepareRawArguments(args any) (json.RawMessage, error) {
	switch typed := args.(type) {
	case nil:
		return json.RawMessage("{}"), nil
	case json.RawMessage:
		if len(typed) == 0 {
			return json.RawMessage("{}"), nil
		}
		return typed, nil
	case []byte:
		return json.RawMessage(typed), nil
	case string:
		return json.RawMessage(typed), nil
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return nil, err
		}
		return encoded, nil
	}
}

func contextFromSignal(signal <-chan struct{}) context.Context {
	if signal == nil {
		return context.Background()
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-signal:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}

func contextDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

func decodeDetails(details json.RawMessage) any {
	if len(details) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(details, &value); err != nil {
		return string(details)
	}
	return value
}

func toolResultText(result ToolResult) string {
	parts := make([]string, 0, len(result.Content))
	for _, block := range result.Content {
		if block.Text != nil {
			parts = append(parts, block.Text.Text)
		}
	}
	if len(parts) == 0 {
		return "tool execution failed"
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------------------
// path mutation queue
// ---------------------------------------------------------------------------

type pathMutationState struct {
	mu     sync.Mutex
	queues map[string]chan struct{}
}

var (
	pathMutationMu     sync.Mutex
	pathMutationStates = map[string]*pathMutationState{}
)

func withPathMutationQueue[T any](key string, fn func() (T, error)) (T, error) {
	if key == "" {
		return fn()
	}
	pathMutationMu.Lock()
	state, ok := pathMutationStates[key]
	if !ok {
		state = &pathMutationState{queues: map[string]chan struct{}{}}
		pathMutationStates[key] = state
	}
	pathMutationMu.Unlock()

	state.mu.Lock()
	previous := state.queues[key]
	done := make(chan struct{})
	state.queues[key] = done
	state.mu.Unlock()

	if previous != nil {
		<-previous
	}
	defer func() {
		close(done)
		state.mu.Lock()
		if state.queues[key] == done {
			delete(state.queues, key)
		}
		state.mu.Unlock()
	}()
	return fn()
}

// WithFileMutationQueue serializes file mutations targeting the same
// environment and canonical path. It forwards to the accepted harness queue so
// the SDK has a single owner for the behavior.
func WithFileMutationQueue[T any](
	env harnesstypes.ExecutionEnv,
	path string,
	fn func() (T, error),
	ctx context.Context,
) (T, error) {
	return harnesstools.WithFileMutationQueue(env, path, fn, ctx)
}
