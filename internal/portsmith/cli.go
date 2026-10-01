// cli.go ports src/cli.ts: the command-line surface for analyze, plan,
// prepare, run, judge-check, verify, status, next, accept and migrate.
//
// The TypeScript source used node:util parseArgs, console output and
// process.exitCode. This Go port preserves the commands, flags, defaults,
// strict numeric validation, JSON projections and exit behavior, but writes
// through the io.Writer pair supplied by cmd/portsmith/main.go so the process
// entry point owns signal cancellation and the final exit status. Runtime
// analyze/help never invoke an external process, so they work with no Node, Go
// or Git on PATH.
package portsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// helpText mirrors the help constant in src/cli.ts. Keep the command list and
// flag spelling in sync with parseCLIArgs.
const helpText = `Portsmith — an inspectable, resumable TypeScript-to-Go migration workbench

portsmith sync --init [--project <target>] [--config <sync.json>]
portsmith sync --upstream <commit> [--project <target>] [--source <upstream-git>] [--out <plan-directory>] [--check | --commit] [--env-file <file>]
portsmith analyze --source <source> --out <analysis.json>
portsmith migrate --plan <plan-directory> --check
portsmith migrate --plan <plan-directory> --commit [--env-file <file>] [--max-attempts 0] [--max-units 1]
portsmith plan --analysis <analysis.json> --out <plan-directory> --revision <revision>
portsmith prepare --plan <plan-directory> --unit <unit-id> --out <task-directory>
portsmith prepare --source <source> --out <task-directory> --revision <revision>
                  --file <relative-file> [--file <test>] --goal <goal>
portsmith prepare --source <pi-source> --out <task-directory> --revision <revision> --example event-stream
portsmith run --task <task-directory> [--env-file <file>] [--feedback <review.md>] [--max-turns <turns>] [--timeout <seconds>]
portsmith judge-check --task <task-directory>
portsmith verify --task <task-directory> [--allow-download]
portsmith status --task <task-directory>
portsmith status --plan <plan-directory> --runs <runs-directory>
portsmith next --plan <plan-directory> --runs <runs-directory>
portsmith accept --task <task-directory> --out <new-export-directory>

prepare options: --rules <rules.md> --go-mod <go.mod> --go-sum <go.sum> --judge <judge-directory>
Uses Pith Coding Agent's native tools, persistent sessions and Go hooks. Local execution is not an OS sandbox.
Default: unlimited model turns, repair attempts and runtime. Positive --max-turns / --max-attempts / --timeout values set budgets; 0 disables them. Ctrl-C preserves progress.
Mature Go dependencies are allowed. Manifests are frozen; --allow-download permits verifier dependency downloads.
analyze/plan do not call models. Review and complete acceptance contracts before execution. No automatic push or publication.
Legacy revision labels are user-supplied; sync requires exact Git hashes and checked upstream blobs.
Sync drafts need reviewed contracts, independent judges and explicit new mappings before model execution.
v2 requires all batches prepared before starting. needs-preparation reports gaps before model calls. --max-units counts complete modules.
`

// maxSafeInteger mirrors JavaScript's Number.isSafeInteger upper bound. It
// keeps the Go port from accepting a budget JavaScript would reject.
const maxSafeInteger = int64(9007199254740991)

// cliRunBudgetError is the exact validation message used by src/cli.ts for the
// run and migrate budgets.
const cliRunBudgetError = "max-turns must be a non-negative integer; timeout must be 0–2147483 seconds; 0 means unlimited"

// cliMigrationLimitError is the exact validation message used by
// src/migrate.ts for the migration limits.
const cliMigrationLimitError = "max-attempts must be non-negative (0 means unlimited); max-units must be positive"

// cliOptions is the parsed command line. Empty strings mean an absent string
// option; maxTurns/timeout/maxAttempts carry the parseArgs defaults.
type cliOptions struct {
	source, out, analysis, revision, plan, unit, runs, goal string
	example, rules, goMod, goSum, judge, task               string
	envFile, feedback                                       string
	files                                                   []string
	maxTurns, timeout, maxAttempts                          string
	maxUnits                                                string
	hasMaxUnits                                             bool
	allowDownload, check, commit, help                      bool
	upstream, project, config                               string
	init                                                    bool
}

// parseCLIArgs approximates node:util parseArgs in strict mode: long flags
// accept `--name value` or `--name=value`, booleans accept only the bare flag,
// `-h`/`--help` are recognized and positional arguments are rejected.
func parseCLIArgs(args []string) (*cliOptions, error) {
	options := &cliOptions{maxTurns: "0", timeout: "0", maxAttempts: "0"}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "-h" || argument == "--help" {
			options.help = true
			continue
		}
		if !strings.HasPrefix(argument, "--") || argument == "--" {
			return nil, fmt.Errorf("Unexpected argument: %s", argument)
		}
		name := argument[2:]
		var inline *string
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			value := name[equals+1:]
			name = name[:equals]
			inline = &value
		}
		value := func() (string, error) {
			if inline != nil {
				return *inline, nil
			}
			if index+1 >= len(args) {
				return "", fmt.Errorf("Option --%s requires a value", name)
			}
			index++
			return args[index], nil
		}
		boolean := func() error {
			if inline != nil {
				return fmt.Errorf("Option --%s does not take a value", name)
			}
			return nil
		}

		var err error
		switch name {
		case "upstream":
			options.upstream, err = value()
		case "project":
			options.project, err = value()
		case "config":
			options.config, err = value()
		case "init":
			if err = boolean(); err == nil {
				options.init = true
			}
		case "source":
			options.source, err = value()
		case "out":
			options.out, err = value()
		case "analysis":
			options.analysis, err = value()
		case "revision":
			options.revision, err = value()
		case "plan":
			options.plan, err = value()
		case "unit":
			options.unit, err = value()
		case "runs":
			options.runs, err = value()
		case "file":
			var item string
			if item, err = value(); err == nil {
				options.files = append(options.files, item)
			}
		case "goal":
			options.goal, err = value()
		case "example":
			options.example, err = value()
		case "rules":
			options.rules, err = value()
		case "go-mod":
			options.goMod, err = value()
		case "go-sum":
			options.goSum, err = value()
		case "judge":
			options.judge, err = value()
		case "task":
			options.task, err = value()
		case "env-file":
			options.envFile, err = value()
		case "feedback":
			options.feedback, err = value()
		case "max-turns":
			options.maxTurns, err = value()
		case "timeout":
			options.timeout, err = value()
		case "max-attempts":
			options.maxAttempts, err = value()
		case "max-units":
			options.maxUnits, err = value()
			options.hasMaxUnits = err == nil
		case "allow-download":
			if err = boolean(); err == nil {
				options.allowDownload = true
			}
		case "check":
			if err = boolean(); err == nil {
				options.check = true
			}
		case "commit":
			if err = boolean(); err == nil {
				options.commit = true
			}
		case "help":
			if err = boolean(); err == nil {
				options.help = true
			}
		default:
			return nil, fmt.Errorf("Unknown option: --%s", name)
		}
		if err != nil {
			return nil, err
		}
	}
	return options, nil
}

// Main implements the process exit behavior of src/cli.ts. It never calls
// os.Exit; cmd/portsmith/main.go owns that so the function stays testable and
// safe to call in-process.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	var command string
	var rest []string
	if len(args) > 0 {
		command = args[0]
		rest = args[1:]
	}
	if command == "" || command == "help" || command == "--help" || command == "-h" {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	options, err := parseCLIArgs(rest)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	if options.help {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	code, err := dispatchCLI(ctx, command, options, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	return code
}

// dispatchCLI routes one parsed command. Errors become exit status 1 in Main.
func dispatchCLI(ctx context.Context, command string, options *cliOptions, stdout io.Writer) (int, error) {
	switch command {
	case "sync":
		r, err := PrepareSync(ctx, SyncOptions{Project: options.project, Config: options.config, Source: options.source, Upstream: options.upstream, Out: options.out, Init: options.init})
		if err != nil {
			return 1, err
		}
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, string(b))
		if (options.commit || options.check) && r.Plan != "" {
			options.plan = r.Plan
			inspected, err := inspectModules(r.Plan)
			if err != nil {
				return 1, err
			}
			if _, err = reviewedSyncMappings(inspected, r.Plan); err != nil {
				return 1, err
			}
			code, err := cliMigrate(ctx, options, stdout)
			if err != nil || code != 0 {
				return code, err
			}
			if options.commit && !options.check {
				if err := advanceSync(ctx, options, r); err != nil {
					return 1, err
				}
			}
			return 0, nil
		}
		if r.Status == "needs-preparation" {
			return 2, nil
		}
		return 0, nil
	case "analyze":
		return cliAnalyze(ctx, options, stdout)
	case "plan":
		return cliPlan(options, stdout)
	case "prepare":
		return cliPrepare(ctx, options, stdout)
	case "status", "next":
		return cliStatus(command, options, stdout)
	case "migrate":
		return cliMigrate(ctx, options, stdout)
	case "judge-check", "verify", "accept", "run":
		return cliTaskCommand(ctx, command, options, stdout)
	default:
		// src/cli.ts resolves the task before rejecting an unknown command, so
		// reproduce that ordering rather than inventing a new message.
		if _, err := requiredFlag("task", options.task); err != nil {
			return 1, err
		}
		return 1, fmt.Errorf("Unknown command: %s", command)
	}
}

// requiredFlag mirrors the `req(name)` helper: a missing or empty string
// option is an error.
func requiredFlag(name, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("Missing --%s", name)
	}
	return value, nil
}

// cliBudget validates --max-turns and --timeout exactly like src/cli.ts,
// including the int32 millisecond bound the TypeScript AbortSignal.timeout
// path needed.
func cliBudget(rawTurns, rawTimeout string) (int, time.Duration, error) {
	turns, err := strconv.ParseInt(rawTurns, 10, 64)
	if err != nil || turns < 0 || turns > maxSafeInteger {
		return 0, 0, errors.New(cliRunBudgetError)
	}
	seconds, err := strconv.ParseInt(rawTimeout, 10, 64)
	if err != nil || seconds < 0 || seconds > 2147483 {
		return 0, 0, errors.New(cliRunBudgetError)
	}
	return int(turns), time.Duration(seconds) * time.Second, nil
}

// cliMigrationLimits validates --max-attempts and optional --max-units. A
// present but non-positive --max-units is rejected here so it is not confused
// with the Go API's zero value meaning "all units".
func cliMigrationLimits(options *cliOptions) (int, int, error) {
	attempts, err := strconv.ParseInt(options.maxAttempts, 10, 64)
	if err != nil || attempts < 0 || attempts > maxSafeInteger {
		return 0, 0, errors.New(cliMigrationLimitError)
	}
	maxUnits := 0
	if options.hasMaxUnits {
		units, err := strconv.ParseInt(options.maxUnits, 10, 64)
		if err != nil || units < 1 || units > maxSafeInteger {
			return 0, 0, errors.New(cliMigrationLimitError)
		}
		maxUnits = int(units)
	}
	return int(attempts), maxUnits, nil
}

// cliAnalyze runs the deterministic analysis and prints the same summary
// object as src/cli.ts.
func cliAnalyze(ctx context.Context, options *cliOptions, stdout io.Writer) (int, error) {
	source, err := requiredFlag("source", options.source)
	if err != nil {
		return 1, err
	}
	out, err := requiredFlag("out", options.out)
	if err != nil {
		return 1, err
	}
	report, err := Analyze(ctx, source)
	if err != nil {
		return 1, err
	}
	if err := saveAnalysis(report, out); err != nil {
		return 1, err
	}
	var analysis Analysis
	if err := json.Unmarshal(report, &analysis); err != nil {
		return 1, err
	}
	lines := 0
	unresolved := 0
	for _, file := range analysis.Files {
		lines += file.Lines
		for _, edge := range file.Imports {
			if edge.Kind == "unresolved" || edge.Kind == "computed" {
				unresolved++
			}
		}
	}
	absolute, err := filepath.Abs(out)
	if err != nil {
		return 1, err
	}
	warnings := analysis.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	summary := struct {
		Files      int      `json:"files"`
		Lines      int      `json:"lines"`
		Cycles     int      `json:"cycles"`
		Unresolved int      `json:"unresolved"`
		Warnings   []string `json:"warnings"`
		Out        string   `json:"out"`
	}{
		Files:      len(analysis.Files),
		Lines:      lines,
		Cycles:     len(analysis.Cycles),
		Unresolved: unresolved,
		Warnings:   warnings,
		Out:        absolute,
	}
	return 0, writeEncodedJSON(stdout, summary)
}

// saveAnalysis mirrors `saveAnalysis` from src/analyze.ts: create the parent
// directory and atomically write the report.
func saveAnalysis(report json.RawMessage, out string) error {
	absolute, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return err
	}
	return AtomicJSON(out, report)
}

// cliPlan writes a reviewable draft plan.
func cliPlan(options *cliOptions, stdout io.Writer) (int, error) {
	analysis, err := requiredFlag("analysis", options.analysis)
	if err != nil {
		return 1, err
	}
	out, err := requiredFlag("out", options.out)
	if err != nil {
		return 1, err
	}
	revision, err := requiredFlag("revision", options.revision)
	if err != nil {
		return 1, err
	}
	encoded, err := CreatePlan(analysis, out, revision)
	if err != nil {
		return 1, err
	}
	var plan Plan
	if err := json.Unmarshal(encoded, &plan); err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "Generated %d draft tasks and found %d package cycles. Review goal, targetPackage, acceptance and dependencies in plan.json.\n", len(plan.Units), len(plan.PackageCycles))
	return 0, nil
}

// cliPrepare snapshots an immutable task either from a plan unit or from an
// explicit source/file/goal selection, including the built-in event-stream
// example.
func cliPrepare(ctx context.Context, options *cliOptions, stdout io.Writer) (int, error) {
	if options.example != "" && options.example != "event-stream" {
		return 1, errors.New("The only built-in example is event-stream")
	}
	out, err := requiredFlag("out", options.out)
	if err != nil {
		return 1, err
	}
	base := PrepareOptions{
		Out:   out,
		Rules: options.rules,
		GoMod: options.goMod,
		GoSum: options.goSum,
		Judge: options.judge,
	}
	var root string
	if options.plan != "" {
		id, err := requiredFlag("unit", options.unit)
		if err != nil {
			return 1, err
		}
		selected, err := selectUnit(options.plan, id, "")
		if err != nil {
			return 1, err
		}
		base.Source = selected.Plan.Source
		base.Revision = selected.Plan.Revision
		base.Files = append(append([]string{}, selected.Unit.Files...), selected.Unit.References...)
		base.Goal = selected.Unit.Goal + "\nAcceptance:\n" + strings.Join(selected.Unit.Acceptance, "\n") + "\nTarget Go package: " + selected.Unit.TargetPackage
		if options.rules == "" {
			base.Rules = filepath.Join(options.plan, "RULEBOOK.md")
		}
		base.Unit = selected.Unit.ID
		base.DependsOn = selected.Unit.DependsOn
		base.PlanDigest = selected.PlanDigest
		root, err = PrepareTask(ctx, base)
		if err != nil {
			return 1, err
		}
	} else {
		source, err := requiredFlag("source", options.source)
		if err != nil {
			return 1, err
		}
		revision, err := requiredFlag("revision", options.revision)
		if err != nil {
			return 1, err
		}
		base.Source = source
		base.Revision = revision
		if options.example == "event-stream" {
			base.Files = []string{
				"packages/ai/src/utils/event-stream.ts",
				"packages/ai/test/event-stream.test.ts",
			}
			base.Goal = EVENT_GOAL
			base.Example = options.example
		} else {
			goal, err := requiredFlag("goal", options.goal)
			if err != nil {
				return 1, err
			}
			base.Files = options.files
			base.Goal = goal
		}
		root, err = PrepareTask(ctx, base)
		if err != nil {
			return 1, err
		}
	}
	fmt.Fprintf(stdout, "Task snapshot created: %s\n", root)
	return 0, nil
}

// cliStatus reports task or plan states. `next` filters the plan projection to
// unblocked, unfinished units.
func cliStatus(command string, options *cliOptions, stdout io.Writer) (int, error) {
	if options.task != "" {
		status, err := TaskStatus(options.task)
		if err != nil {
			return 1, err
		}
		return 0, writePrettyJSON(stdout, status)
	}
	planRoot, err := requiredFlag("plan", options.plan)
	if err != nil {
		return 1, err
	}
	runs, err := requiredFlag("runs", options.runs)
	if err != nil {
		return 1, err
	}
	states, err := PlanStatus(planRoot, runs)
	if err != nil {
		return 1, err
	}
	if command != "next" {
		return 0, writePrettyJSON(stdout, states)
	}
	var entries []planStatusEntry
	if err := json.Unmarshal(states, &entries); err != nil {
		return 1, err
	}
	filtered := make([]planStatusEntry, 0, len(entries))
	for _, entry := range entries {
		if len(entry.BlockedBy) == 0 && entry.State != "behavior_verified" && entry.State != "accepted" {
			filtered = append(filtered, entry)
		}
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return 1, err
	}
	return 0, writePrettyJSON(stdout, encoded)
}

// cliMigrate prepares or executes a migration. The model connection is created
// lazily inside the generate callback, so --check is offline and needs no key.
func cliMigrate(ctx context.Context, options *cliOptions, stdout io.Writer) (int, error) {
	plan, err := requiredFlag("plan", options.plan)
	if err != nil {
		return 1, err
	}
	maxTurns, timeout, err := cliBudget(options.maxTurns, options.timeout)
	if err != nil {
		return 1, err
	}
	maxAttempts, maxUnits, err := cliMigrationLimits(options)
	if err != nil {
		return 1, err
	}
	progress := func(message string) { fmt.Fprintln(stdout, message) }
	var connection *ModelConfig
	generate := func(runCtx context.Context, root, feedback string) (RunReport, error) {
		if connection == nil {
			if err := LoadLocalEnv(options.envFile); err != nil {
				return RunReport{}, err
			}
			model, err := ConfiguredModel()
			if err != nil {
				return RunReport{}, err
			}
			connection = &model
			fmt.Fprintf(stdout, "Model: %s; maximum output: %d tokens; working context: %d tokens\n", model.ID, model.MaxTokens, model.ContextWindow)
		}
		return RunPort(runCtx, RunOptions{
			Root:       root,
			Model:      *connection,
			MaxTurns:   maxTurns,
			Timeout:    timeout,
			Download:   options.allowDownload,
			Feedback:   feedback,
			OnProgress: progress,
		})
	}
	result, err := Migrate(ctx, MigrationOptions{
		Plan:        plan,
		Commit:      options.commit,
		Check:       options.check,
		Download:    options.allowDownload,
		MaxAttempts: maxAttempts,
		MaxUnits:    maxUnits,
		Generate:    generate,
		OnProgress:  progress,
	})
	if err != nil {
		return 1, err
	}
	if err := writePrettyJSON(stdout, result); err != nil {
		return 1, err
	}
	var status struct {
		Status string `json:"status"`
	}
	if len(result) > 0 {
		_ = json.Unmarshal(result, &status)
	}
	if status.Status == "needs-preparation" {
		return 2, nil
	}
	return 0, nil
}

// cliTaskCommand covers the commands that operate on one prepared task under
// its exclusive lock.
func cliTaskCommand(ctx context.Context, command string, options *cliOptions, stdout io.Writer) (int, error) {
	task, err := requiredFlag("task", options.task)
	if err != nil {
		return 1, err
	}
	root, err := filepath.Abs(task)
	if err != nil {
		return 1, err
	}
	code := 0
	err = WithLock(ctx, root, func() error {
		switch command {
		case "judge-check":
			raw, err := JudgeCheck(ctx, root)
			if err != nil {
				return err
			}
			return writePrettyJSON(stdout, raw)
		case "verify":
			report, err := VerifyPort(ctx, root, options.allowDownload)
			if err != nil {
				return err
			}
			if err := cliPrintVerification(stdout, report); err != nil {
				return err
			}
			if report.Status != "tests_passed" && report.Status != "behavior_verified" {
				code = 1
			}
			return nil
		case "accept":
			out, err := requiredFlag("out", options.out)
			if err != nil {
				return err
			}
			exported, err := AcceptTask(root, out)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Exported: %s\n", exported)
			return nil
		case "run":
			return cliRun(ctx, options, root, stdout, &code)
		}
		return fmt.Errorf("Unknown command: %s", command)
	})
	if err != nil {
		return 1, err
	}
	return code, nil
}

// cliRun resolves the model and executes one durable Pith session.
func cliRun(ctx context.Context, options *cliOptions, root string, stdout io.Writer, code *int) error {
	maxTurns, timeout, err := cliBudget(options.maxTurns, options.timeout)
	if err != nil {
		return err
	}
	feedback := ""
	if options.feedback != "" {
		data, err := os.ReadFile(options.feedback)
		if err != nil {
			return err
		}
		feedback = string(data)
	}
	if err := LoadLocalEnv(options.envFile); err != nil {
		return err
	}
	model, err := ConfiguredModel()
	if err != nil {
		return err
	}
	turns := "unlimited turns"
	if maxTurns > 0 {
		turns = fmt.Sprintf("maximum %d turns", maxTurns)
	}
	fmt.Fprintf(stdout, "Model: %s; %s\n", model.ID, turns)
	report, err := RunPort(ctx, RunOptions{
		Root:       root,
		Model:      model,
		MaxTurns:   maxTurns,
		Timeout:    timeout,
		Download:   options.allowDownload,
		Feedback:   feedback,
		OnProgress: func(message string) { fmt.Fprintln(stdout, message) },
	})
	if err != nil {
		return err
	}
	if report.Error != "" {
		fmt.Fprintln(stdout, report.Error)
	}
	fmt.Fprintf(stdout, "%s · %d turns; session saved. Acceptance still requires independent verification.\n", report.Status, report.Turns)
	if report.Status != "candidate_ready" {
		*code = 1
	}
	return nil
}

// cliPrintVerification mirrors the verify command output: one line per phase,
// the tail of a failed phase log and the non-parity reminder.
func cliPrintVerification(stdout io.Writer, report Verification) error {
	for _, phase := range report.Phases {
		passed := phase.Result.Code != nil && *phase.Result.Code == 0
		label := "failed"
		if passed {
			label = "passed"
		}
		if _, err := fmt.Fprintf(stdout, "%s: %s\n", phase.Name, label); err != nil {
			return err
		}
		if !passed {
			log := phase.Result.Log
			if len(log) > 12000 {
				log = log[len(log)-12000:]
			}
			if _, err := fmt.Fprintln(stdout, log); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(stdout, "%s · %d built-in oracle cases; not proof of full parity\n", report.Status, report.OracleCases)
	return err
}

// writePrettyJSON indents a JSON document without re-encoding it, so numbers
// and string escapes survive exactly.
func writePrettyJSON(w io.Writer, raw []byte) error {
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, raw, "", "  "); err != nil {
		return err
	}
	buffer.WriteByte('\n')
	_, err := w.Write(buffer.Bytes())
	return err
}

// writeEncodedJSON writes a value like JSON.stringify(value, null, 2) with a
// trailing newline and without HTML escaping.
func writeEncodedJSON(w io.Writer, value any) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	_, err := w.Write(buffer.Bytes())
	return err
}
