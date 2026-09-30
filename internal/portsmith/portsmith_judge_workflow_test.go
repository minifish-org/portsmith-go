package portsmith_test

import (
	"context"
	ps "github.com/minifish-org/portsmith-go/internal/portsmith"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func miniPlan(t *testing.T, version int) (string, string) {
	t.Helper()
	d := t.TempDir()
	project := filepath.Join(d, "target")
	source := filepath.Join(d, "source")
	put(t, source, "LICENSE", "fixture license")
	put(t, source, "value.ts", "export const Value=42;\n")
	put(t, project, ".gitignore", ".portsmith/\n")
	put(t, project, "go.mod", "module example.com/candidate\n\ngo 1.24\n")
	gitCmd(t, project, "init", "-q")
	gitCmd(t, project, "config", "user.name", "Portsmith Judge")
	gitCmd(t, project, "config", "user.email", "judge@example.invalid")
	gitCmd(t, project, "config", "commit.gpgsign", "false")
	gitCmd(t, project, "add", ".")
	gitCmd(t, project, "commit", "-qm", "Initial fixture")
	plan := filepath.Join(project, "migration")
	put(t, plan, "go.mod", "module example.com/fixture-material\n\ngo 1.24\n")
	put(t, plan, "RULEBOOK.md", "Preserve behavior.\n")
	put(t, plan, "contract.md", "Implement Value() int returning 42; write a self-test.\n")
	put(t, plan, "judges/port/value_judge_test.go", miniJudge)
	a, e := ps.Analyze(context.Background(), source)
	must(t, e)
	put(t, plan, "analysis.json", string(a))
	outputs := []string{"port/value.go", "port/value_test.go"}
	spec := map[string]any{"outputs": outputs, "tests": []string{"TestPortsmithJudgeValue"}, "judge": "judges", "contract": "contract.md"}
	common := map[string]any{"version": version, "source": source, "revision": "fixture", "analysisSha256": digest(a)}
	w := map[string]any{"version": version, "project": "..", "runs": ".portsmith/runs", "bootstrap": []string{"migration"}}
	if version == 2 {
		common["modules"] = []any{map[string]any{"id": "app", "dependsOn": []string{}, "batches": []string{"runtime"}}}
		common["batches"] = []any{map[string]any{"id": "runtime", "module": "app", "dependsOn": []string{}, "sources": []string{"value.ts"}, "references": []string{}, "outputs": outputs, "behaviors": []string{"value"}, "acceptance": []string{"42"}}}
		spec["id"] = "port"
		spec["sources"] = []string{"value.ts"}
		spec["goal"] = "Port Value"
		w["startPolicy"] = "all-prepared"
		w["batches"] = map[string]any{"runtime": map[string]any{"status": "ready", "steps": []any{spec}}}
	} else {
		common["units"] = []any{map[string]any{"id": "value", "goal": "Port Value", "targetPackage": "port", "files": []string{"value.ts"}, "references": []string{}, "dependsOn": []string{}, "acceptance": []string{"Value returns 42"}, "notes": []string{}}}
		common["packageCycles"] = []any{}
		w["units"] = map[string]any{"value": spec}
	}
	put(t, plan, "plan.json", encode(t, common))
	put(t, plan, "workflow.json", encode(t, w))
	return project, plan
}

func TestPortsmithJudgeAutomaticMigrationAndResume(t *testing.T) {
	for _, version := range []int{1, 2} {
		project, plan := miniPlan(t, version)
		calls := 0
		opts := ps.MigrationOptions{Plan: plan, Commit: true, Check: true, MaxAttempts: 3, Generate: func(ctx context.Context, p, feedback string) (ps.RunReport, error) {
			calls++
			must(t, ps.WriteCandidate(p, "port/value.go", []byte(goodCode)))
			must(t, ps.WriteCandidate(p, "port/value_test.go", []byte(selfTest)))
			return ps.RunReport{Status: "candidate_ready"}, nil
		}}
		b, e := ps.Migrate(context.Background(), opts)
		must(t, e)
		if decode(t, b)["status"] != "ready" || calls != 0 {
			t.Fatal("preflight called generator", string(b))
		}
		opts.Check = false
		b, e = ps.Migrate(context.Background(), opts)
		must(t, e)
		if decode(t, b)["status"] != "complete" || calls != 1 {
			t.Fatal("migration not complete", string(b), calls)
		}
		if !strings.Contains(string(raw(t, filepath.Join(project, "port/value.go"))), "return 42") {
			t.Fatal("missing integrated code")
		}
		if gitCmd(t, project, "status", "--porcelain") != "" {
			t.Fatal("migration left dirty project")
		}
		head := gitCmd(t, project, "rev-parse", "HEAD")
		b, e = ps.Migrate(context.Background(), opts)
		must(t, e)
		if calls != 1 || decode(t, b)["status"] != "complete" || gitCmd(t, project, "rev-parse", "HEAD") != head {
			t.Fatal("resume repeated generation/commit")
		}
	}
}

func TestPortsmithJudgeRepairBudgetAndFrozenPlan(t *testing.T) {
	project, plan := miniPlan(t, 2)
	calls := 0
	opts := ps.MigrationOptions{Plan: plan, Commit: true, MaxAttempts: 1, Generate: func(ctx context.Context, p, feedback string) (ps.RunReport, error) {
		calls++
		if calls == 1 {
			must(t, ps.WriteCandidate(p, "port/value.go", []byte(badCode)))
			must(t, ps.WriteCandidate(p, "port/value_test.go", []byte(selfTest)))
		} else {
			if !strings.Contains(feedback, "behavior_failed") && !strings.Contains(feedback, "wrong value") {
				t.Error("repair feedback lost diagnostics")
			}
			must(t, ps.WriteCandidate(p, "port/value.go", []byte(goodCode)))
		}
		return ps.RunReport{Status: "candidate_ready"}, nil
	}}
	b, e := ps.Migrate(context.Background(), opts)
	if e != nil && !strings.Contains(e.Error(), "limit") && !strings.Contains(e.Error(), "attempt") {
		t.Fatal(e)
	}
	if e == nil && decode(t, b)["status"] == "complete" {
		t.Fatal("accepted wrong candidate")
	}
	if _, e = os.Stat(filepath.Join(project, "port/value.go")); !os.IsNotExist(e) {
		t.Fatal("integrated unverified output")
	}
	b, e = ps.Migrate(context.Background(), opts)
	must(t, e)
	if calls != 2 || decode(t, b)["status"] != "complete" {
		t.Fatal("did not continue saved candidate", calls, string(b))
	}
	put(t, plan, "contract.md", "Changed contract\n")
	if _, e = ps.Migrate(context.Background(), opts); e == nil {
		t.Fatal("trusted receipts after contract change")
	}
}

func TestPortsmithJudgeAcceptanceReceipt(t *testing.T) {
	p := prepared(t)
	candidate(t, p, goodCode)
	v, e := ps.VerifyPort(context.Background(), p, false)
	must(t, e)
	if v.Status != "behavior_verified" {
		t.Fatal(v.Status)
	}
	out, e := ps.AcceptTask(p, filepath.Join(t.TempDir(), "export"))
	must(t, e)
	status, e := ps.TaskStatus(p)
	must(t, e)
	if decode(t, status)["state"] != "accepted" {
		t.Fatal(string(status))
	}
	put(t, out, "value.go", badCode)
	status, e = ps.TaskStatus(p)
	must(t, e)
	if decode(t, status)["state"] != "export_changed" {
		t.Fatal("did not detect modified accepted export", string(status))
	}
}

func TestPortsmithJudgeAdditiveBaseline(t *testing.T) {
	project, plan := miniPlan(t, 2)
	base := "package base\nfunc Number() int {return 42}\n"
	put(t, project, "internal/base/value.go", base)
	gitCmd(t, project, "add", ".")
	gitCmd(t, project, "commit", "-qm", "Frozen baseline")
	revision := gitCmd(t, project, "rev-parse", "HEAD")
	w := decode(t, raw(t, filepath.Join(plan, "workflow.json")))
	w["journal"] = ".portsmith/additive/modules.json"
	w["baseline"] = map[string]any{"commit": revision, "files": []any{map[string]any{"name": "internal/base/value.go", "sha256": digest([]byte(base))}}}
	put(t, plan, "workflow.json", encode(t, w))
	opts := ps.MigrationOptions{Plan: plan, Commit: true, MaxAttempts: 1, Generate: func(ctx context.Context, p, feedback string) (ps.RunReport, error) {
		if string(raw(t, filepath.Join(p, "candidate/internal/base/value.go"))) != base {
			t.Fatal("missing frozen seed")
		}
		if e := ps.WriteCandidate(p, "internal/base/value.go", []byte("changed")); e == nil {
			t.Fatal("baseline writable")
		}
		must(t, ps.WriteCandidate(p, "port/value.go", []byte("package port\nimport \"example.com/candidate/internal/base\"\nfunc Value() int {return base.Number()}\n")))
		must(t, ps.WriteCandidate(p, "port/value_test.go", []byte(selfTest)))
		return ps.RunReport{Status: "candidate_ready"}, nil
	}}
	b, e := ps.Migrate(context.Background(), opts)
	must(t, e)
	if decode(t, b)["status"] != "complete" {
		t.Fatal(string(b))
	}
	if string(raw(t, filepath.Join(project, "internal/base/value.go"))) != base {
		t.Fatal("baseline was replaced")
	}
	if _, e = os.Stat(filepath.Join(project, ".portsmith/additive/modules.json")); e != nil {
		t.Fatal(e)
	}
	put(t, project, "internal/base/value.go", base+"// edited\n")
	if _, e = ps.Migrate(context.Background(), opts); e == nil {
		t.Fatal("trusted changed baseline")
	}
}
