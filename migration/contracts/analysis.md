# analysis

Use embedded TypeScript 5.9.3 through pinned Goja, not regular expressions as an AST replacement. Supply an explicit read-only TypeScript resolution/config host (ts.sys is a getter in the compiler bundle; assigning it does not work), with filesystem callbacks and tsconfig parsing/module-resolution support; transpile the small bridge to ES5 if necessary. Support imports, reexports, import type, require, dynamic/computed imports, .js-to-.ts NodeNext resolution, aliases, workspace package classification, declaration files, SCC cycles, exports, lines and hashes exactly as analyze.ts. Preserve wildcard fallback and node_modules alias distinctions. Hidden/build/dependency trees and symlinks are skipped. Analyze returns the original version-1 JSON schema with source canonicalized. CreatePlan writes the original draft plan shape, rules, README, dependencies, package-cycle warnings and exact analysis hash. This is deterministic analysis, never a model call. Cancel CPU-bound Goja work with runtime.Interrupt and release resources. Do not cache reports across changed files.

Read all source-owned files and the supplied upstream tests. Earlier outputs in the same module remain writable; preserve their acceptance. Implement additional internal helpers freely within the manifest.

## Shared Go boundary

The following is a compile-only API contract for all stages. Implement only the functions owned by this stage and dependencies required by it; later-stage functions are not required early. JSON-returning functions preserve the matching TypeScript schema. Types may be placed in the owning source file. This empty implementation is a negative control and MUST NOT be copied into product code.

```go
// Compile-only contract. Deliberately unimplemented; never ship this file.
package portsmith

import (
	"context"
	"encoding/json"
	"errors"
	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	"io"
	"time"
)

var errStub = errors.New("unimplemented negative control")

type File struct {
	Name   string
	Data   []byte
	SHA256 string
}
type ProcessOptions struct {
	Command        string
	Args           []string
	Cwd            string
	Timeout        time.Duration
	Download, Race bool
	MaxLogChars    int
}
type ProcessResult struct {
	Code      *int   `json:"code"`
	TimedOut  bool   `json:"timedOut"`
	Truncated bool   `json:"truncated"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Log       string `json:"log"`
}
type ModelConfig struct {
	ID            string
	BaseURL       string
	APIKey        string `json:"-"`
	ContextWindow int
	MaxTokens     int
	Thinking      string
}
type PrepareOptions struct {
	Source, Out, Revision, Goal, Example, Rules, GoMod, GoSum, Judge, Unit, PlanDigest, Contract string
	Files, DependsOn, WritableFiles, RequiredJudgeTests                                          []string
	Seed, Initial, JudgeFiles                                                                    []File
	Race, ModuleTask                                                                             bool
}
type Phase struct {
	Name   string        `json:"name"`
	Result ProcessResult `json:"result"`
}
type Verification struct {
	Version          int     `json:"version"`
	Verifier         string  `json:"verifier"`
	At               string  `json:"at"`
	Fingerprint      string  `json:"fingerprint"`
	Status           string  `json:"status"`
	OracleCases      int     `json:"oracleCases"`
	Independent      bool    `json:"independent"`
	FullParityProven bool    `json:"fullParityProven"`
	Phases           []Phase `json:"phases"`
}
type CurrentVerificationResult struct {
	Current bool
	Report  Verification
}
type RunOptions struct {
	Root       string
	Model      ModelConfig
	AgentDir   string
	Feedback   string
	MaxTurns   int
	Timeout    time.Duration
	Download   bool
	OnProgress func(string)
	// Optional dependency injection for offline tests. Nil selects Pith's native provider.
	StreamFn agenttypes.StreamFn
}
type RunReport struct {
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	StopReason  string `json:"stopReason,omitempty"`
	SessionFile string `json:"sessionFile"`
	Resumed     bool   `json:"resumed"`
	Turns       int    `json:"turns"`
}
type MigrationOptions struct {
	Plan                    string
	Commit, Check, Download bool
	MaxAttempts, MaxUnits   int
	Generate                func(context.Context, string, string) (RunReport, error)
	OnProgress              func(string)
}

func Hash([]byte) string                                                 { return "" }
func RelativeName(string) (string, error)                                { return "", errStub }
func CheckedFile(string, string) (string, error)                         { return "", errStub }
func AtomicJSON(string, any) error                                       { return errStub }
func WithLock(context.Context, string, func() error) error               { return errStub }
func CleanEnv(bool, bool) map[string]string                              { return nil }
func Execute(context.Context, ProcessOptions) ProcessResult              { return ProcessResult{} }
func LoadLocalEnv(string) error                                          { return errStub }
func ConfiguredModel() (ModelConfig, error)                              { return ModelConfig{}, errStub }
func Analyze(context.Context, string) (json.RawMessage, error)           { return nil, errStub }
func CreatePlan(string, string, string) (json.RawMessage, error)         { return nil, errStub }
func PrepareTask(context.Context, PrepareOptions) (string, error)        { return "", errStub }
func LoadTask(string) (json.RawMessage, error)                           { return nil, errStub }
func WriteCandidate(string, string, []byte) error                        { return errStub }
func EditCandidate(string, string, string, string) error                 { return errStub }
func CandidateFiles(string) ([]File, error)                              { return nil, errStub }
func Fingerprint(string) (string, error)                                 { return "", errStub }
func VerifyPort(context.Context, string, bool) (Verification, error)     { return Verification{}, errStub }
func CurrentVerification(string) (*CurrentVerificationResult, error)     { return nil, errStub }
func JudgeCheck(context.Context, string) (json.RawMessage, error)        { return nil, errStub }
func EventStreamOracle(context.Context, string) (json.RawMessage, error) { return nil, errStub }
func TaskStatus(string) (json.RawMessage, error)                         { return nil, errStub }
func PlanStatus(string, string) (json.RawMessage, error)                 { return nil, errStub }
func AcceptTask(string, string) (string, error)                          { return "", errStub }
func RunPort(context.Context, RunOptions) (RunReport, error)             { return RunReport{}, errStub }
func Migrate(context.Context, MigrationOptions) (json.RawMessage, error) { return nil, errStub }
func Main(context.Context, []string, io.Writer, io.Writer) int           { return 1 }

```
