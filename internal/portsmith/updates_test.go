package portsmith

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func updateFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	source := filepath.Join(root, "source")
	plan := filepath.Join(project, "migration/increment")
	testPut(t, project, "go.mod", "module example.com/update\n\ngo 1.24\n")
	testPut(t, project, ".gitignore", ".portsmith/\n")
	old := "package value\nfunc Value() int {return 41}\n"
	testPut(t, project, "internal/value/value.go", old)
	testPut(t, project, "internal/value/old_test.go", "package value\nimport \"testing\"\nfunc TestOldRegression(t *testing.T){if Value()<0{t.Fatal(\"negative\")}}\n")
	wfInitRepo(t, project)
	wfGit(t, project, "add", ".")
	wfGit(t, project, "commit", "-qm", "baseline")
	base := wfGit(t, project, "rev-parse", "HEAD")
	testPut(t, source, "LICENSE", "fixture\n")
	testPut(t, source, "value.ts", "export const value=42;\n")
	a, e := Analyze(context.Background(), source)
	if e != nil {
		t.Fatal(e)
	}
	testPut(t, plan, "analysis.json", string(a))
	testPut(t, plan, "go.mod", "module example.com/material\n\ngo 1.24\n")
	testPut(t, plan, "RULEBOOK.md", "Preserve behavior\n")
	testPut(t, plan, "contract.md", "Update Value to return 42.\n")
	testPut(t, plan, "judge/internal/value/portsmith_judge_increment_test.go", "package value\nimport \"testing\"\nfunc TestPortsmithJudgeIncrement(t *testing.T){if Value()!=42{t.Fatal(\"not updated\")}}\n")
	outputs := []string{"internal/value/value.go", "internal/value/increment_test.go"}
	p := modulePlan{Version: 2, Source: source, Revision: "new", AnalysisSHA256: Hash(a), Modules: []moduleDef{{ID: "increment", DependsOn: []string{}, Batches: []string{"value"}}}, Batches: []batchDef{{ID: "value", Module: "increment", DependsOn: []string{}, Sources: []string{"value.ts"}, References: []string{}, Outputs: outputs, Behaviors: []string{"42"}, Acceptance: []string{"42"}}}}
	w := moduleWorkflow{Version: 2, Project: "../..", Runs: ".portsmith/increment/runs", Journal: ".portsmith/increment/modules.json", StartPolicy: "all-prepared", Bootstrap: []string{"migration/increment"}, Updates: &baselineConfig{Commit: base, Files: []NamedDigest{{Name: "internal/value/value.go", SHA256: Hash([]byte(old))}}}, Baseline: &baselineConfig{Commit: base, Files: []NamedDigest{{Name: "internal/value/old_test.go", SHA256: Hash([]byte("package value\nimport \"testing\"\nfunc TestOldRegression(t *testing.T){if Value()<0{t.Fatal(\"negative\")}}\n"))}}}, Batches: map[string]batchConfig{"value": {Status: "ready", Steps: []stepDef{{ID: "port", Sources: []string{"value.ts"}, Goal: "Update Value", Contract: "contract.md", Judge: "judge", Outputs: outputs, Tests: []string{"TestPortsmithJudgeIncrement"}}}}}}
	if e = AtomicJSON(filepath.Join(plan, "plan.json"), p); e != nil {
		t.Fatal(e)
	}
	if e = AtomicJSON(filepath.Join(plan, "workflow.json"), w); e != nil {
		t.Fatal(e)
	}
	return project, plan
}
func updateGenerator(t *testing.T) func(context.Context, string, string) (RunReport, error) {
	return func(ctx context.Context, root, feedback string) (RunReport, error) {
		b, e := readCheckedBytes(filepath.Join(root, "candidate"), "internal/value/value.go")
		if e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(string(b), "41") {
			t.Fatal("original writable code missing")
		}
		e = WriteCandidate(root, "internal/value/value.go", []byte("package value\nfunc Value() int {return 42}\n"))
		if e != nil {
			return RunReport{}, e
		}
		e = WriteCandidate(root, "internal/value/increment_test.go", []byte("package value\nimport \"testing\"\nfunc TestIncrement(t *testing.T){if Value()!=42{t.Fatal(\"bad\")}}\n"))
		return RunReport{Status: "candidate_ready"}, e
	}
}
func TestUpdateExistingFileAndResume(t *testing.T) {
	project, plan := updateFixture(t)
	calls := 0
	g := updateGenerator(t)
	options := MigrationOptions{Plan: plan, Commit: true, MaxAttempts: 1, Generate: func(ctx context.Context, r, f string) (RunReport, error) { calls++; return g(ctx, r, f) }}
	b, e := Migrate(context.Background(), options)
	if e != nil {
		t.Fatal(e)
	}
	if wfDecode(t, b)["status"] != "complete" {
		t.Fatal(string(b))
	}
	before := wfGit(t, project, "rev-parse", "HEAD")
	b, e = Migrate(context.Background(), options)
	if e != nil {
		t.Fatal(e)
	}
	if calls != 1 || wfGit(t, project, "rev-parse", "HEAD") != before {
		t.Fatal("repeated generation or commit")
	}
	if wfGit(t, project, "status", "--porcelain") != "" {
		t.Fatal("dirty target")
	}
	data, e := os.ReadFile(filepath.Join(project, "internal/value/value.go"))
	if e != nil || !strings.Contains(string(data), "42") {
		t.Fatal("file not replaced", e)
	}
}
func TestUpdateRejectsDriftAndMissingAuthorization(t *testing.T) {
	for _, kind := range []string{"drift", "hash", "missing"} {
		t.Run(kind, func(t *testing.T) {
			project, plan := updateFixture(t)
			if kind == "drift" {
				testPut(t, project, "internal/value/value.go", "package value\nfunc Value() int{return 99}\n")
			}
			if kind != "drift" {
				w, e := readJSON[moduleWorkflow](plan, "workflow.json")
				if e != nil {
					t.Fatal(e)
				}
				if kind == "hash" {
					w.Updates.Files[0].SHA256 = strings.Repeat("0", 64)
				} else {
					w.Updates = nil
				}
				if e = AtomicJSON(filepath.Join(plan, "workflow.json"), w); e != nil {
					t.Fatal(e)
				}
			}
			calls := 0
			_, e := Migrate(context.Background(), MigrationOptions{Plan: plan, Commit: true, MaxAttempts: 1, Generate: func(ctx context.Context, r, f string) (RunReport, error) {
				calls++
				return updateGenerator(t)(ctx, r, f)
			}})
			if e == nil {
				t.Fatal("unsafe update accepted")
			}
			if kind != "missing" && calls != 0 {
				t.Fatal("generated before rejecting drift")
			}
		})
	}
}

func TestUpdateRecoveryPreservesOriginalAndVerifiedHashes(t *testing.T) {
	for _, mode := range []string{"partial", "after-commit", "tamper"} {
		t.Run(mode, func(t *testing.T) {
			project, plan := updateFixture(t)
			// Interrupt the commit after successful integration. Integration test
			// failures now reopen the final step instead of leaving this phase.
			testPut(t, project, ".git/hooks/pre-commit", "#!/bin/sh\nif test -f .portsmith/reject; then echo simulated commit interruption >&2; exit 1; fi\n")
			if e := os.Chmod(filepath.Join(project, ".git/hooks/pre-commit"), 0o755); e != nil {
				t.Fatal(e)
			}
			calls := 0
			g := updateGenerator(t)
			o := MigrationOptions{Plan: plan, Commit: true, MaxAttempts: 1, Generate: func(ctx context.Context, r, f string) (RunReport, error) {
				calls++
				testPut(t, project, ".portsmith/reject", "yes")
				return g(ctx, r, f)
			}}
			_, e := Migrate(context.Background(), o)
			if e == nil || !strings.Contains(e.Error(), "simulated commit interruption") {
				t.Fatal("expected pending commit interruption", e)
			}
			state, e := readJSON[moduleState](project, ".portsmith/increment/modules.json")
			if e != nil {
				t.Fatal(e)
			}
			if state.Pending == nil || len(state.Pending.Before) != 1 {
				t.Fatal("original hashes not journaled")
			}
			if e = os.Remove(filepath.Join(project, ".portsmith/reject")); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "partial":
				testPut(t, project, "internal/value/value.go", "package value\nfunc Value() int {return 41}\n")
			case "tamper":
				testPut(t, project, "internal/value/value.go", "package value\nfunc Value() int {return 99}\n")
			case "after-commit":
				names := []string{}
				for _, f := range state.Pending.Files {
					names = append(names, f.Name)
				}
				if _, e = commitFiles(context.Background(), project, names, state.Pending.Message); e != nil {
					t.Fatal(e)
				}
			}
			b, e := Migrate(context.Background(), o)
			if mode == "tamper" {
				if e == nil {
					t.Fatal("overwrote user edits")
				}
				data, _ := os.ReadFile(filepath.Join(project, "internal/value/value.go"))
				if !strings.Contains(string(data), "99") {
					t.Fatal("user edit lost")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if wfDecode(t, b)["status"] != "complete" || calls != 1 {
				t.Fatal("recovery repeated generation", calls, string(b))
			}
			if wfGit(t, project, "status", "--porcelain") != "" {
				t.Fatal("recovery left dirty target")
			}
		})
	}
}
