package portsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cliMain runs Main in-process and returns the exit code with both streams.
func cliMain(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLIHelpListsEveryCommand(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		code, stdout, stderr := cliMain(t, args...)
		if code != 0 {
			t.Fatalf("%v exit %d: %s", args, code, stderr)
		}
		for _, command := range []string{"analyze", "plan", "prepare", "run", "judge-check", "verify", "status", "next", "accept", "migrate"} {
			if !strings.Contains(stdout, command) {
				t.Errorf("%v help missing %q", args, command)
			}
		}
	}
}

func TestCLIUnknownCommandRejected(t *testing.T) {
	code, _, stderr := cliMain(t, "frobnicate")
	if code == 0 {
		t.Fatal("unknown command accepted")
	}
	if !strings.Contains(stderr, "Missing --task") && !strings.Contains(stderr, "Unknown command") {
		t.Fatalf("unexpected error: %q", stderr)
	}
	// With a task path the source reports the unknown command itself.
	code, _, stderr = cliMain(t, "frobnicate", "--task", t.TempDir())
	if code == 0 || !strings.Contains(stderr, "Unknown command: frobnicate") {
		t.Fatalf("unknown command with task: code=%d stderr=%q", code, stderr)
	}
}

func TestCLIRejectsMissingAndUnknownOptions(t *testing.T) {
	if code, _, stderr := cliMain(t, "analyze", "--source", t.TempDir()); code == 0 || !strings.Contains(stderr, "Missing --out") {
		t.Fatalf("missing out: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := cliMain(t, "analyze", "--nope", "x"); code == 0 || !strings.Contains(stderr, "Unknown option") {
		t.Fatalf("unknown option: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := cliMain(t, "analyze", "positional"); code == 0 || !strings.Contains(stderr, "Unexpected argument") {
		t.Fatalf("positional: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := cliMain(t, "analyze", "--allow-download=1"); code == 0 || !strings.Contains(stderr, "does not take a value") {
		t.Fatalf("boolean value: code=%d stderr=%q", code, stderr)
	}
}

func TestCLIAnalyzeWritesReportAndSummary(t *testing.T) {
	source := t.TempDir()
	writeAnalysisFixture(t, source)
	out := filepath.Join(t.TempDir(), "nested", "analysis.json")
	code, stdout, stderr := cliMain(t, "analyze", "--source", source, "--out", out)
	if code != 0 {
		t.Fatalf("analyze exit %d: %s", code, stderr)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report Analysis
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Version != 1 || len(report.Files) == 0 || len(report.Cycles) == 0 {
		t.Fatalf("unexpected report: version=%d files=%d cycles=%d", report.Version, len(report.Files), len(report.Cycles))
	}
	var summary struct {
		Files      int      `json:"files"`
		Lines      int      `json:"lines"`
		Cycles     int      `json:"cycles"`
		Unresolved int      `json:"unresolved"`
		Warnings   []string `json:"warnings"`
		Out        string   `json:"out"`
	}
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("summary not JSON: %v\n%s", err, stdout)
	}
	if summary.Files != len(report.Files) || summary.Cycles != len(report.Cycles) {
		t.Fatalf("summary mismatch: %+v", summary)
	}
	absolute, err := filepath.Abs(out)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Out != absolute {
		t.Fatalf("summary out %q != %q", summary.Out, absolute)
	}
	if summary.Unresolved < 1 {
		t.Fatalf("expected unresolved imports, got %d", summary.Unresolved)
	}
}

func TestCLIPlanPrepareAndNext(t *testing.T) {
	source := t.TempDir()
	writeAnalysisFixture(t, source)
	analysisFile := filepath.Join(t.TempDir(), "analysis.json")
	code, _, stderr := cliMain(t, "analyze", "--source", source, "--out", analysisFile)
	if code != 0 {
		t.Fatalf("analyze: %s", stderr)
	}
	planDir := filepath.Join(t.TempDir(), "plan")
	code, stdout, stderr := cliMain(t, "plan", "--analysis", analysisFile, "--out", planDir, "--revision", "rev-1")
	if code != 0 {
		t.Fatalf("plan: %s", stderr)
	}
	if !strings.Contains(stdout, "draft tasks") {
		t.Fatalf("plan output: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(planDir, "plan.json")); err != nil {
		t.Fatal(err)
	}
	// `next` over an untouched runs directory keeps unblocked, unfinished units
	// and never crashes when no task exists yet.
	runs := t.TempDir()
	code, stdout, stderr = cliMain(t, "next", "--plan", planDir, "--runs", runs)
	if code != 0 {
		t.Fatalf("next: %s", stderr)
	}
	var entries []planStatusEntry
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		t.Fatalf("next output not JSON: %v\n%s", err, stdout)
	}
	for _, entry := range entries {
		if len(entry.BlockedBy) != 0 {
			t.Fatalf("next returned a blocked unit: %+v", entry)
		}
	}
}

func TestParseCLIArgsRepeatableFileAndInline(t *testing.T) {
	options, err := parseCLIArgs([]string{
		"--file=a.ts", "--file", "b.ts", "--goal=port it",
		"--max-turns", "3", "--allow-download", "--check",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(options.files) != 2 || options.files[0] != "a.ts" || options.files[1] != "b.ts" {
		t.Fatalf("files: %v", options.files)
	}
	if options.goal != "port it" || options.maxTurns != "3" {
		t.Fatalf("values: %+v", options)
	}
	if !options.allowDownload || !options.check || options.commit {
		t.Fatalf("booleans: %+v", options)
	}
	if _, err := parseCLIArgs([]string{"--allow-download=yes"}); err == nil {
		t.Fatal("boolean inline value accepted")
	}
	if _, err := parseCLIArgs([]string{"--source"}); err == nil {
		t.Fatal("missing value accepted")
	}
}

func TestCLIBudgetValidation(t *testing.T) {
	if turns, timeout, err := cliBudget("0", "0"); err != nil || turns != 0 || timeout != 0 {
		t.Fatalf("unlimited rejected: %d %v %v", turns, timeout, err)
	}
	if _, _, err := cliBudget("-1", "0"); err == nil {
		t.Fatal("negative turns accepted")
	}
	if _, _, err := cliBudget("1.5", "0"); err == nil {
		t.Fatal("fractional turns accepted")
	}
	if _, _, err := cliBudget("1", "2147484"); err == nil {
		t.Fatal("over-long timeout accepted")
	}
	if _, timeout, err := cliBudget("1", "2147483"); err != nil || timeout != 2147483*time.Second {
		t.Fatalf("boundary timeout rejected: %v %v", timeout, err)
	}
	if _, _, err := cliMigrationLimits(&cliOptions{maxAttempts: "0"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cliMigrationLimits(&cliOptions{maxAttempts: "0", maxUnits: "0", hasMaxUnits: true}); err == nil {
		t.Fatal("zero max-units accepted")
	}
	if attempts, units, err := cliMigrationLimits(&cliOptions{maxAttempts: "2", maxUnits: "3", hasMaxUnits: true}); err != nil || attempts != 2 || units != 3 {
		t.Fatalf("valid limits: %d %d %v", attempts, units, err)
	}
}

func TestCLIPrepareExampleValidationAndFiles(t *testing.T) {
	if code, _, stderr := cliMain(t, "prepare", "--out", t.TempDir(), "--example", "other"); code == 0 || !strings.Contains(stderr, "only built-in example") {
		t.Fatalf("example rejection: code=%d stderr=%q", code, stderr)
	}
	source := t.TempDir()
	testPut(t, source, "LICENSE", "fixture license\n")
	testPut(t, source, "value.ts", "export const value = 1;\n")
	out := filepath.Join(t.TempDir(), "task")
	code, stdout, stderr := cliMain(t,
		"prepare", "--source", source, "--out", out, "--revision", "rev-1",
		"--file", "value.ts", "--goal", "Port value")
	if code != 0 {
		t.Fatalf("prepare: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "Task snapshot created") {
		t.Fatalf("prepare output: %q", stdout)
	}
	task, err := LoadTask(out)
	if err != nil {
		t.Fatal(err)
	}
	var manifest PortTask
	if err := json.Unmarshal(task, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Revision != "rev-1" || manifest.Goal != "Port value" || len(manifest.Files) != 2 {
		t.Fatalf("manifest: %+v", manifest)
	}
}

func TestDemoTaskRequiresExample(t *testing.T) {
	if _, _, err := DemoTask(context.Background(), DemoOptions{Example: "   "}, nil); err == nil {
		t.Fatal("empty example accepted")
	}
	if _, _, err := DemoTask(context.Background(), DemoOptions{Example: t.TempDir(), Out: filepath.Join(t.TempDir(), "task")}, nil); err == nil {
		t.Fatal("example without source accepted")
	}
}

func TestMainWritesErrorsToStderrNotStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"migrate", "--plan", t.TempDir(), "--check"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("invalid migration plan accepted: %s", stdout.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("error leaked to stdout: %q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatal("missing stderr message")
	}
}
