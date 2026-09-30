// workflow_test.go adds candidate-owned coverage for the version-1 and
// version-2 migration workflows: preflight policies, cumulative independent
// gates, repair, pending-transaction recovery, additive baselines, frozen
// assets and ownership collisions. The independent judge exercises the primary
// path; these tests pin the remaining source-owned behavior.
package portsmith

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func wfGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func wfDecode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func wfInitRepo(t *testing.T, root string) {
	t.Helper()
	wfGit(t, root, "init", "-q")
	wfGit(t, root, "config", "user.name", "Portsmith Test")
	wfGit(t, root, "config", "user.email", "test@example.invalid")
	wfGit(t, root, "config", "commit.gpgsign", "false")
}

func wfFeatCommits(t *testing.T, root string) []string {
	t.Helper()
	output := wfGit(t, root, "log", "--format=%s")
	var commits []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "feat: port ") {
			commits = append(commits, line)
		}
	}
	return commits
}

// wfModuleFixture builds a two-module version-2 workflow whose foundation
// batch starts partial and whose consumer batch is ready.
type wfModuleFixture struct {
	t               *testing.T
	project, source string
	plan            string
	planDoc         modulePlan
	workflow        moduleWorkflow
	first, second   stepDef
	third           stepDef
	analysisRaw     json.RawMessage
}

func wfNewModuleFixture(t *testing.T) *wfModuleFixture {
	t.Helper()
	f := &wfModuleFixture{t: t}
	dir := t.TempDir()
	f.project = filepath.Join(dir, "project")
	f.source = filepath.Join(dir, "source")
	f.plan = filepath.Join(f.project, "migration")
	testPut(t, f.project, "LICENSE", "fixture\n")
	wfInitRepo(t, f.project)
	wfGit(t, f.project, "add", "LICENSE")
	wfGit(t, f.project, "commit", "-qm", "initial")
	testPut(t, f.project, ".gitignore", ".portsmith/\n")
	testPut(t, f.project, "go.mod", "module example.com/modules\n\ngo 1.24\n")
	testPut(t, f.source, "LICENSE", "MIT fixture\n")
	testPut(t, f.source, "value.ts", "export const value=42;\n")
	raw, err := Analyze(context.Background(), f.source)
	if err != nil {
		t.Fatal(err)
	}
	f.analysisRaw = raw
	testPut(t, f.plan, "analysis.json", string(raw))
	testPut(t, f.plan, "go.mod", "module fixture.local/judges\n\ngo 1.24\n")
	testPut(t, f.plan, "RULEBOOK.md", "Preserve fixture behavior\n")

	foundationOutputs := []string{"alpha/value.go", "alpha/value_test.go", "alpha/helper.go", "alpha/helper_test.go", "alpha/data.json"}
	consumerOutputs := []string{"beta/value.go", "beta/value_test.go"}
	f.first = f.addStep("First", "alpha", foundationOutputs[:2], "Value()!=42")
	f.second = f.addStep("Second", "alpha", foundationOutputs[2:], "helper()!=42")
	f.third = f.addStep("Third", "beta", consumerOutputs, "Value()!=84")

	f.planDoc = modulePlan{
		Version:        2,
		Source:         f.source,
		Revision:       "fixture",
		AnalysisSHA256: Hash(raw),
		Modules: []moduleDef{
			{ID: "alpha", DependsOn: []string{}, Batches: []string{"foundation"}},
			{ID: "beta", DependsOn: []string{"alpha"}, Batches: []string{"consumer"}},
		},
		Batches: []batchDef{
			{ID: "foundation", Module: "alpha", DependsOn: []string{}, Sources: []string{"value.ts"}, References: []string{}, Outputs: foundationOutputs, Behaviors: []string{"value"}, Acceptance: []string{"offline tests"}},
			{ID: "consumer", Module: "beta", DependsOn: []string{}, Sources: []string{"value.ts"}, References: []string{}, Outputs: consumerOutputs, Behaviors: []string{"value"}, Acceptance: []string{"offline tests"}},
		},
	}
	f.workflow = moduleWorkflow{
		Version:     2,
		Project:     "..",
		Runs:        ".portsmith/runs",
		StartPolicy: "available-steps",
		Bootstrap:   []string{"migration", "go.mod", ".gitignore"},
		Batches: map[string]batchConfig{
			"foundation": {Status: "partial", Reason: "Second contract pending", Steps: []stepDef{f.first}},
			"consumer":   {Status: "ready", Steps: []stepDef{f.third}},
		},
	}
	f.save()
	return f
}

func (f *wfModuleFixture) addStep(id, pkg string, outputs []string, expr string) stepDef {
	f.t.Helper()
	name := "TestPortsmithJudge" + id
	testPut(f.t, f.plan, id+".md", id+": value must be correct; candidate unit tests required\n")
	testPut(f.t, f.plan, "judges/"+id+"/"+pkg+"/portsmith_judge_"+id+"_test.go",
		"package "+pkg+"\nimport \"testing\"\nfunc "+name+"(t *testing.T){if "+expr+"{t.Fatal(\"wrong behavior\")}}\n")
	return stepDef{
		ID:       id,
		Sources:  []string{"value.ts"},
		Goal:     id,
		Contract: id + ".md",
		Judge:    "judges/" + id,
		Outputs:  outputs,
		Tests:    []string{name},
	}
}

func (f *wfModuleFixture) save() {
	f.t.Helper()
	if err := AtomicJSON(filepath.Join(f.plan, "plan.json"), f.planDoc); err != nil {
		f.t.Fatal(err)
	}
	if err := AtomicJSON(filepath.Join(f.plan, "workflow.json"), f.workflow); err != nil {
		f.t.Fatal(err)
	}
}

func (f *wfModuleFixture) ready() {
	f.t.Helper()
	cfg := f.workflow.Batches["foundation"]
	cfg.Status = "ready"
	cfg.Reason = ""
	cfg.Steps = []stepDef{f.first, f.second}
	f.workflow.Batches["foundation"] = cfg
	f.save()
}

func (f *wfModuleFixture) generate(root string, wrong bool) (RunReport, error) {
	f.t.Helper()
	_, _, task, err := loadTask(root)
	if err != nil {
		return RunReport{}, err
	}
	switch {
	case strings.HasSuffix(task.Unit, "/First"):
		if err := WriteCandidate(root, "alpha/value.go", []byte("package alpha\nfunc Value() int {return 42}\n")); err != nil {
			return RunReport{}, err
		}
		if err := WriteCandidate(root, "alpha/value_test.go", []byte("package alpha\nimport \"testing\"\nfunc TestValue(t *testing.T){if Value()<0{t.Fatal(\"negative\")}}\n")); err != nil {
			return RunReport{}, err
		}
	case strings.HasSuffix(task.Unit, "/Second"):
		helper := 42
		if wrong {
			helper = 41
		}
		if err := WriteCandidate(root, "alpha/value.go", []byte("package alpha\nfunc Value() int {return helper()}\n")); err != nil {
			return RunReport{}, err
		}
		if err := WriteCandidate(root, "alpha/helper.go", []byte("package alpha\nfunc helper() int {return "+itoa(helper)+"}\n")); err != nil {
			return RunReport{}, err
		}
		if err := WriteCandidate(root, "alpha/helper_test.go", []byte("package alpha\nimport \"testing\"\nfunc TestHelper(t *testing.T){if helper()<0{t.Fatal(\"negative\")}}\n")); err != nil {
			return RunReport{}, err
		}
		if err := WriteCandidate(root, "alpha/data.json", []byte("{\"fixture\":true}\n")); err != nil {
			return RunReport{}, err
		}
	case strings.HasSuffix(task.Unit, "/Third"):
		if err := WriteCandidate(root, "beta/value.go", []byte("package beta\nimport \"example.com/modules/alpha\"\nfunc Value()int{return alpha.Value()*2}\n")); err != nil {
			return RunReport{}, err
		}
		if err := WriteCandidate(root, "beta/value_test.go", []byte("package beta\nimport \"testing\"\nfunc TestValue(t *testing.T){if Value()<0{t.Fatal(\"negative\")}}\n")); err != nil {
			return RunReport{}, err
		}
	default:
		return RunReport{}, errors.New("unexpected unit " + task.Unit)
	}
	return RunReport{Status: "candidate_ready"}, nil
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

func wfNoModel(_ context.Context, _, _ string) (RunReport, error) {
	return RunReport{}, errors.New("must not call model")
}

// TestWorkflowV2ModuleCheckpoints pins the version-2 checkpoint policy:
// accepted steps are saved without committing the module, a repaired
// same-module step sees earlier files, each module commits once, resume does
// not regenerate and an interrupted commit is recovered.
func TestWorkflowV2ModuleCheckpoints(t *testing.T) {
	f := wfNewModuleFixture(t)
	ctx := context.Background()

	checked, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Check: true, Generate: wfNoModel})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, checked)["status"] != "partially-ready" {
		t.Fatalf("preflight status %s", checked)
	}
	if commits := wfGit(t, f.project, "rev-list", "--count", "HEAD"); commits != "1" {
		t.Fatalf("preflight created commits: %s", commits)
	}
	calls := 0
	first, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Commit: true, Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
		calls++
		return f.generate(root, false)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, first)["status"] != "needs-preparation" || calls != 1 {
		t.Fatalf("step checkpoint not paused: %s calls=%d", first, calls)
	}
	if _, err := os.Stat(filepath.Join(f.project, "alpha/value.go")); !os.IsNotExist(err) {
		t.Fatal("module committed before all steps passed")
	}
	recheck, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Check: true, Generate: wfNoModel})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, recheck)["status"] != "needs-preparation" {
		t.Fatalf("check ignored preparation gap: %s", recheck)
	}
	if _, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Commit: true, Generate: wfNoModel}); err != nil {
		t.Fatalf("resume regenerated accepted step: %v", err)
	}
	f.ready()
	second := 0
	result, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Commit: true, Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
		calls++
		wrong := false
		if strings.HasSuffix(root, "Second") {
			second++
			wrong = second == 1
		}
		return f.generate(root, wrong)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, result)["status"] != "complete" || calls != 4 {
		t.Fatalf("cumulative module run failed: %s calls=%d", result, calls)
	}
	commits := wfFeatCommits(t, f.project)
	if len(commits) != 2 || commits[0] != "feat: port beta" || commits[1] != "feat: port alpha" {
		t.Fatalf("unexpected module commits: %v", commits)
	}
	if status := wfGit(t, f.project, "status", "--porcelain"); status != "" {
		t.Fatalf("dirty tree: %s", status)
	}
	if body, err := os.ReadFile(filepath.Join(f.project, "alpha/value.go")); err != nil || !strings.Contains(string(body), "helper") {
		t.Fatalf("same-module integration lost earlier edits: %v %s", err, body)
	}
	if _, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Commit: true, Generate: wfNoModel}); err != nil {
		t.Fatal(err)
	}
	if commits := wfFeatCommits(t, f.project); len(commits) != 2 {
		t.Fatalf("resume duplicated module commit: %v", commits)
	}

	// Simulate power loss after the module commit but before the receipt save.
	journal := filepath.Join(f.project, ".portsmith", "modules.json")
	state, err := readJSON[moduleState](f.project, ".portsmith/modules.json")
	if err != nil {
		t.Fatal(err)
	}
	done := state.Modules[len(state.Modules)-1]
	state.Modules = state.Modules[:len(state.Modules)-1]
	last := state.Steps[len(state.Steps)-1]
	state.Pending = &modulePending{
		Module:      done.ID,
		Base:        wfGit(t, f.project, "rev-parse", "HEAD^"),
		Files:       done.Files,
		Message:     wfGit(t, f.project, "log", "-1", "--format=%B"),
		Task:        last.Task,
		Fingerprint: last.Fingerprint,
		Staging:     ".portsmith/runs/" + done.ID + "-integration",
	}
	if err := AtomicJSON(journal, state); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Commit: true, Generate: wfNoModel}); err != nil {
		t.Fatalf("pending recovery regenerated: %v", err)
	}
	if commits := wfFeatCommits(t, f.project); len(commits) != 2 {
		t.Fatalf("pending recovery duplicated commit: %v", commits)
	}
}

// TestWorkflowV2AllPreparedGaps pins the default all-prepared policy: no task
// directory, bootstrap commit or model call happens while any batch is
// incomplete, even when the first module is fully prepared.
func TestWorkflowV2AllPreparedGaps(t *testing.T) {
	f := wfNewModuleFixture(t)
	f.workflow.StartPolicy = ""
	consumer := f.workflow.Batches["consumer"]
	consumer.Status = "planned"
	consumer.Reason = "Consumer judges pending"
	consumer.Steps = []stepDef{}
	f.workflow.Batches["consumer"] = consumer
	f.save()
	before := wfGit(t, f.project, "status", "--porcelain")
	calls := 0
	for _, check := range []bool{true, false} {
		report, err := Migrate(context.Background(), MigrationOptions{Plan: f.plan, Check: check, Commit: !check, Generate: func(context.Context, string, string) (RunReport, error) {
			calls++
			return RunReport{}, errors.New("must not call model")
		}})
		if err != nil {
			t.Fatal(err)
		}
		decoded := wfDecode(t, report)
		if decoded["status"] != "needs-preparation" || decoded["canStart"] != false {
			t.Fatalf("all-prepared gap not blocked: %s", report)
		}
		preparation := decoded["preparation"].(map[string]any)
		if preparation["ready"] != false || preparation["readyBatches"] != float64(0) {
			t.Fatalf("preparation report wrong: %s", report)
		}
		blocked := decoded["blocked"].([]any)
		modules := []string{}
		for _, entry := range blocked {
			modules = append(modules, entry.(map[string]any)["module"].(string))
		}
		if len(modules) != 2 || modules[0] != "alpha" || modules[1] != "beta" {
			t.Fatalf("blocked modules %v", modules)
		}
	}
	if calls != 0 {
		t.Fatal("model called during preparation block")
	}
	if status := wfGit(t, f.project, "status", "--porcelain"); status != before {
		t.Fatalf("preparation block mutated the tree: %s", status)
	}
	if commits := wfGit(t, f.project, "rev-list", "--count", "HEAD"); commits != "1" {
		t.Fatalf("preparation block created commits: %s", commits)
	}
	if _, err := os.Stat(filepath.Join(f.project, ".portsmith", "modules.json")); !os.IsNotExist(err) {
		t.Fatal("preparation block created a journal")
	}
	f.ready()
	later, err := Migrate(context.Background(), MigrationOptions{Plan: f.plan, Check: true, Generate: wfNoModel})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, later)["canStart"] != false {
		t.Fatalf("later gap hidden by complete first module: %s", later)
	}
	consumer = f.workflow.Batches["consumer"]
	consumer.Status = "ready"
	consumer.Reason = ""
	consumer.Steps = []stepDef{f.third}
	f.workflow.Batches["consumer"] = consumer
	f.save()
	ready, err := Migrate(context.Background(), MigrationOptions{Plan: f.plan, Check: true, Generate: wfNoModel})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, ready)["status"] != "ready" || wfDecode(t, ready)["canStart"] != true {
		t.Fatalf("complete plan not ready: %s", ready)
	}
}

// TestWorkflowFrozenAssets pins immutable static assets: hash drift, writable
// declarations, seed injection and integration bytes.
func TestWorkflowFrozenAssets(t *testing.T) {
	f := wfNewModuleFixture(t)
	f.ready()
	name := "alpha/frozen.json"
	data := "{\"catalog\":42}\n"
	testPut(t, f.plan, "catalog.json", data)
	f.planDoc.Batches[0].Outputs = append(f.planDoc.Batches[0].Outputs, name)
	cfg := f.workflow.Batches["foundation"]
	cfg.Steps[0].Assets = []stepAsset{{Source: "catalog.json", Target: name, SHA256: Hash([]byte(data))}}
	f.workflow.Batches["foundation"] = cfg
	f.save()
	check := func() (json.RawMessage, error) {
		return Migrate(context.Background(), MigrationOptions{Plan: f.plan, Check: true, Generate: wfNoModel})
	}
	testPut(t, f.plan, "catalog.json", data+" ")
	if _, err := check(); err == nil || !strings.Contains(err.Error(), "Static asset changed") {
		t.Fatalf("tampered asset accepted: %v", err)
	}
	testPut(t, f.plan, "catalog.json", data)
	cfg = f.workflow.Batches["foundation"]
	cfg.Steps[1].Outputs = append(cfg.Steps[1].Outputs, name)
	f.workflow.Batches["foundation"] = cfg
	f.save()
	if _, err := check(); err == nil || !strings.Contains(err.Error(), "Static assets cannot be declared") {
		t.Fatalf("asset treated as writable: %v", err)
	}
	cfg = f.workflow.Batches["foundation"]
	cfg.Steps[1].Outputs = cfg.Steps[1].Outputs[:len(cfg.Steps[1].Outputs)-1]
	f.workflow.Batches["foundation"] = cfg
	f.save()
	steps := 0
	result, err := Migrate(context.Background(), MigrationOptions{Plan: f.plan, Commit: true, Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
		steps++
		if body, err := os.ReadFile(filepath.Join(root, "candidate", name)); err != nil || string(body) != data {
			t.Fatalf("asset not injected as a frozen seed: %v %s", err, body)
		}
		if err := WriteCandidate(root, name, []byte("{}")); err == nil {
			t.Fatal("static asset was writable")
		}
		return f.generate(root, false)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, result)["status"] != "complete" || steps != 3 {
		t.Fatalf("asset migration failed: %s steps=%d", result, steps)
	}
	if body, err := os.ReadFile(filepath.Join(f.project, name)); err != nil || string(body) != data {
		t.Fatalf("asset bytes changed during integration: %v %s", err, body)
	}
	if status := wfGit(t, f.project, "status", "--porcelain"); status != "" {
		t.Fatalf("dirty tree: %s", status)
	}
}

// TestWorkflowAdditiveBaseline pins the additive journal and frozen baseline.
func TestWorkflowAdditiveBaseline(t *testing.T) {
	f := wfNewModuleFixture(t)
	name := "packages/existing/value.go"
	data := "package existing\nconst Value = 42\n"
	testPut(t, f.project, name, data)
	wfGit(t, f.project, "add", name)
	wfGit(t, f.project, "commit", "-qm", "accepted baseline")
	base := wfGit(t, f.project, "rev-parse", "HEAD")
	f.workflow.Journal = ".portsmith/sdk/modules.json"
	f.workflow.Runs = ".portsmith/sdk/runs"
	f.workflow.Baseline = &baselineConfig{Commit: base, Files: []NamedDigest{{Name: name, SHA256: Hash([]byte(data))}}}
	f.save()
	old := "{\"legacy\":\"must remain untouched\"}\n"
	testPut(t, f.project, ".portsmith/modules.json", old)
	calls := 0
	generate := func(ctx context.Context, root, feedback string) (RunReport, error) {
		calls++
		if body, err := os.ReadFile(filepath.Join(root, "candidate", name)); err != nil || string(body) != data {
			t.Fatalf("baseline not injected: %v %s", err, body)
		}
		if err := WriteCandidate(root, name, []byte("changed")); err == nil {
			t.Fatal("baseline file was writable")
		}
		return f.generate(root, false)
	}
	result, err := Migrate(context.Background(), MigrationOptions{Plan: f.plan, Commit: true, Generate: generate})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, result)["status"] != "needs-preparation" || calls != 1 {
		t.Fatalf("additive step not checkpointed: %s calls=%d", result, calls)
	}
	if _, err := Migrate(context.Background(), MigrationOptions{Plan: f.plan, Commit: true, Generate: func(context.Context, string, string) (RunReport, error) {
		return RunReport{}, errors.New("must reuse accepted step")
	}}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("resume regenerated accepted additive step: %d", calls)
	}
	if body, err := os.ReadFile(filepath.Join(f.project, ".portsmith", "modules.json")); err != nil || string(body) != old {
		t.Fatalf("default journal overwritten: %v %s", err, body)
	}
	state, err := readJSON[moduleState](f.project, ".portsmith/sdk/modules.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Steps) != 1 {
		t.Fatalf("additive journal steps: %d", len(state.Steps))
	}
	testPut(t, f.project, name, data+"// drift\n")
	if _, err := inspectModules(f.plan); err == nil || !strings.Contains(err.Error(), "baseline changed") {
		t.Fatalf("baseline drift trusted: %v", err)
	}
	testPut(t, f.project, name, data)
	f.workflow.Baseline.Files[0].SHA256 = strings.Repeat("0", 64)
	f.save()
	if _, err := inspectModules(f.plan); err == nil || !strings.Contains(err.Error(), "baseline changed") {
		t.Fatalf("baseline hash mismatch trusted: %v", err)
	}
	f.workflow.Baseline.Files[0].SHA256 = Hash([]byte(data))
	f.workflow.Journal = ".portsmith/modules.json"
	f.save()
	if _, err := inspectModules(f.plan); err == nil || !strings.Contains(err.Error(), "separate journal") {
		t.Fatalf("shared journal trusted: %v", err)
	}
}

// TestWorkflowOwnershipCollisions pins module output ownership and bootstrap
// boundaries.
func TestWorkflowOwnershipCollisions(t *testing.T) {
	conflict := wfNewModuleFixture(t)
	conflict.planDoc.Batches[1].Outputs = append(conflict.planDoc.Batches[1].Outputs, "alpha/value.go")
	consumer := conflict.workflow.Batches["consumer"]
	consumer.Steps[0].Outputs = append(consumer.Steps[0].Outputs, "alpha/value.go")
	conflict.workflow.Batches["consumer"] = consumer
	conflict.save()
	if _, err := inspectModules(conflict.plan); err == nil || !strings.Contains(err.Error(), "Module output conflict") {
		t.Fatalf("cross-module output collision accepted: %v", err)
	}

	bootstrap := wfNewModuleFixture(t)
	bootstrap.workflow.Bootstrap = append(bootstrap.workflow.Bootstrap, "alpha")
	bootstrap.save()
	if _, err := inspectModules(bootstrap.plan); err == nil || !strings.Contains(err.Error(), "bootstrap must not include product outputs") {
		t.Fatalf("bootstrap/product collision accepted: %v", err)
	}

	asset := wfNewModuleFixture(t)
	testPut(t, asset.plan, "catalog.json", "{}\n")
	cfg := asset.workflow.Batches["foundation"]
	cfg.Steps[0].Assets = []stepAsset{{Source: "catalog.json", Target: "alpha/value.go", SHA256: Hash([]byte("{}\n"))}}
	asset.workflow.Batches["foundation"] = cfg
	asset.save()
	if _, err := inspectModules(asset.plan); err == nil || !strings.Contains(err.Error(), "Static asset") {
		t.Fatalf("asset/writable collision accepted: %v", err)
	}
}

// wfV1Fixture builds the two-unit version-1 pipeline from the upstream test.
type wfV1Fixture struct {
	t               *testing.T
	project, source string
	plan            string
}

func wfNewV1Fixture(t *testing.T) *wfV1Fixture {
	t.Helper()
	f := &wfV1Fixture{t: t}
	dir := t.TempDir()
	f.project = filepath.Join(dir, "target")
	f.source = filepath.Join(dir, "source")
	f.plan = filepath.Join(f.project, "migration")
	testPut(t, f.project, "LICENSE", "fixture license\n")
	wfInitRepo(t, f.project)
	wfGit(t, f.project, "add", "LICENSE")
	wfGit(t, f.project, "commit", "-qm", "initial")
	testPut(t, f.project, ".gitignore", ".portsmith/\n")
	testPut(t, f.project, "go.mod", "module example.com/pipeline\n\ngo 1.24\n")
	testPut(t, f.source, "LICENSE", "MIT fixture\n")
	testPut(t, f.source, "alpha/value.ts", "export const value=42;\n")
	testPut(t, f.source, "beta/value.ts", "import {value} from '../alpha/value.ts'; export const twice=value*2;\n")
	raw, err := Analyze(context.Background(), f.source)
	if err != nil {
		t.Fatal(err)
	}
	testPut(t, f.plan, "analysis.json", string(raw))
	testPut(t, f.plan, "go.mod", "module example.com/fixtures\n\ngo 1.24\n")
	testPut(t, f.plan, "RULEBOOK.md", "Preserve behavior.\n")
	testPut(t, f.plan, "alpha.md", "alpha API fixed by test fixture\n")
	testPut(t, f.plan, "beta.md", "beta API fixed by test fixture\n")
	testPut(t, f.plan, "judges/alpha/alpha/portsmith_judge_test.go",
		"package alpha\nimport \"testing\"\nfunc TestPortsmithJudgeAlpha(t *testing.T){if Value != 42 {t.Fatal(\"incorrect behavior\")}}\n")
	testPut(t, f.plan, "judges/beta/beta/portsmith_judge_test.go",
		"package beta\nimport \"testing\"\nfunc TestPortsmithJudgeBeta(t *testing.T){if Value() != 84 {t.Fatal(\"incorrect behavior\")}}\n")
	planDoc := Plan{
		Version:        1,
		Source:         f.source,
		Revision:       "fixture",
		AnalysisSHA256: Hash(raw),
		Units: []Unit{
			{ID: "alpha", Goal: "alpha returns expected value", TargetPackage: "alpha", Files: []string{"alpha/value.ts"}, References: []string{}, DependsOn: []string{}, Acceptance: []string{"alpha returns expected value"}, Notes: []string{}},
			{ID: "beta", Goal: "beta returns expected value", TargetPackage: "beta", Files: []string{"beta/value.ts"}, References: []string{"alpha/value.ts"}, DependsOn: []string{"alpha"}, Acceptance: []string{"beta returns expected value"}, Notes: []string{}},
		},
		PackageCycles: [][]string{},
	}
	if err := AtomicJSON(filepath.Join(f.plan, "plan.json"), planDoc); err != nil {
		t.Fatal(err)
	}
	workflow := migrationWorkflow{
		Version:   1,
		Project:   "..",
		Runs:      ".portsmith/tasks",
		Bootstrap: []string{".gitignore", "go.mod", "migration"},
		Units: map[string]unitConfig{
			"alpha": {Contract: "alpha.md", Judge: "judges/alpha", Outputs: []string{"alpha/value.go", "alpha/value_test.go"}, Tests: []string{"TestPortsmithJudgeAlpha"}},
			"beta":  {Contract: "beta.md", Judge: "judges/beta", Outputs: []string{"beta/value.go", "beta/value_test.go"}, Tests: []string{"TestPortsmithJudgeBeta"}},
		},
	}
	if err := AtomicJSON(filepath.Join(f.plan, "workflow.json"), workflow); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *wfV1Fixture) generate(root string, alphaValue int) (RunReport, error) {
	f.t.Helper()
	_, _, task, err := loadTask(root)
	if err != nil {
		return RunReport{}, err
	}
	switch task.Unit {
	case "alpha":
		if err := WriteCandidate(root, "alpha/value.go", []byte("package alpha\nconst Value="+itoa(alphaValue)+"\n")); err != nil {
			return RunReport{}, err
		}
		if err := WriteCandidate(root, "alpha/value_test.go", []byte("package alpha\nimport \"testing\"\nfunc TestCandidate(t *testing.T){if Value<0{t.Fatal(\"negative\")}}\n")); err != nil {
			return RunReport{}, err
		}
	case "beta":
		if _, err := os.ReadFile(filepath.Join(root, "candidate/alpha/value.go")); err != nil {
			return RunReport{}, err
		}
		if err := WriteCandidate(root, "alpha/value.go", []byte("package alpha\nconst Value=99\n")); err == nil {
			return RunReport{}, errors.New("beta wrote a prior-module seed")
		}
		if err := WriteCandidate(root, "beta/value.go", []byte("package beta\nimport \"example.com/pipeline/alpha\"\nfunc Value()int{return alpha.Value*2}\n")); err != nil {
			return RunReport{}, err
		}
		if err := WriteCandidate(root, "beta/value_test.go", []byte("package beta\nimport \"testing\"\nfunc TestCandidate(t *testing.T){if Value()<0{t.Fatal(\"negative\")}}\n")); err != nil {
			return RunReport{}, err
		}
	default:
		return RunReport{}, errors.New("unexpected unit " + task.Unit)
	}
	return RunReport{Status: "candidate_ready"}, nil
}

// TestWorkflowV1PipelineAndRecovery pins preflight, cumulative repair, per-unit
// commits, resume and interrupted-commit recovery for version 1.
func TestWorkflowV1PipelineAndRecovery(t *testing.T) {
	f := wfNewV1Fixture(t)
	ctx := context.Background()
	before := wfGit(t, f.project, "rev-parse", "HEAD")
	ready, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Check: true, Generate: wfNoModel})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, ready)["status"] != "ready" || wfGit(t, f.project, "rev-parse", "HEAD") != before {
		t.Fatalf("v1 preflight mutated the repository: %s", ready)
	}
	calls := 0
	result, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Commit: true, Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
		calls++
		if calls == 1 {
			return f.generate(root, 41)
		}
		return f.generate(root, 42)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, result)["status"] != "complete" || calls != 3 {
		t.Fatalf("v1 pipeline failed: %s calls=%d", result, calls)
	}
	if count := wfGit(t, f.project, "rev-list", "--count", "HEAD"); count != "4" {
		t.Fatalf("expected initial+prep+alpha+beta commits, got %s", count)
	}
	if status := wfGit(t, f.project, "status", "--porcelain"); status != "" {
		t.Fatalf("dirty tree: %s", status)
	}
	receipt, err := os.ReadFile(filepath.Join(f.project, "migration/results/beta.json"))
	if err != nil || !strings.Contains(string(receipt), "behavior_verified") {
		t.Fatalf("missing acceptance receipt: %v %s", err, receipt)
	}
	head := wfGit(t, f.project, "rev-parse", "HEAD")
	if _, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Commit: true, Generate: wfNoModel}); err != nil {
		t.Fatal(err)
	}
	if wfGit(t, f.project, "rev-parse", "HEAD") != head {
		t.Fatal("v1 resume repeated a commit")
	}
	// Power loss after commit, before the completion receipt.
	state, err := readJSON[MigrationState](f.project, ".portsmith/migrate.json")
	if err != nil {
		t.Fatal(err)
	}
	last := state.Completed[len(state.Completed)-1]
	state.Completed = state.Completed[:len(state.Completed)-1]
	state.Pending = &MigrationPending{
		ID:          last.ID,
		Base:        wfGit(t, f.project, "rev-parse", "HEAD^"),
		Files:       last.Files,
		Message:     wfGit(t, f.project, "log", "-1", "--format=%B"),
		Fingerprint: "unused-in-committed-recovery",
	}
	if err := AtomicJSON(filepath.Join(f.project, ".portsmith", "migrate.json"), state); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, MigrationOptions{Plan: f.plan, Commit: true, Generate: wfNoModel}); err != nil {
		t.Fatalf("v1 commit recovery failed: %v", err)
	}
	if count := wfGit(t, f.project, "rev-list", "--count", "HEAD"); count != "4" {
		t.Fatalf("recovery duplicated the commit: %s", count)
	}
}

// TestWorkflowStateStatusProjections pins taskStatus and planStatus.
func TestWorkflowStateStatusProjections(t *testing.T) {
	dir, source := workspaceFixture(t)
	analysisFile := filepath.Join(dir, "analysis.json")
	raw, err := Analyze(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	testPut(t, dir, "analysis.json", string(raw))
	planRoot := filepath.Join(dir, "plan")
	planJSON, err := CreatePlan(analysisFile, planRoot, "test")
	if err != nil {
		t.Fatal(err)
	}
	var plan Plan
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		t.Fatal(err)
	}
	plan.Units[0].Acceptance = []string{"answer=42"}
	if err := AtomicJSON(filepath.Join(planRoot, "plan.json"), plan); err != nil {
		t.Fatal(err)
	}
	selected, err := selectUnit(planRoot, plan.Units[0].ID, source)
	if err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(dir, "runs")
	taskRoot, err := PrepareTask(context.Background(), PrepareOptions{
		Source:     source,
		Out:        filepath.Join(runs, plan.Units[0].ID),
		Revision:   "test",
		Goal:       "answer",
		Files:      []string{"value.ts"},
		GoMod:      workspaceMod(t, dir),
		Unit:       plan.Units[0].ID,
		PlanDigest: selected.PlanDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := TaskStatus(taskRoot)
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, status)["state"] != "prepared" {
		t.Fatalf("fresh task state: %s", status)
	}
	planReport, err := PlanStatus(planRoot, runs)
	if err != nil {
		t.Fatal(err)
	}
	var entries []planStatusEntry
	if err := json.Unmarshal(planReport, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].State != "prepared" {
		t.Fatalf("plan status: %s", planReport)
	}
	plan.Units[0].Goal = "changed scope"
	if err := AtomicJSON(filepath.Join(planRoot, "plan.json"), plan); err != nil {
		t.Fatal(err)
	}
	planReport, err = PlanStatus(planRoot, runs)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planReport, &entries); err != nil {
		t.Fatal(err)
	}
	if entries[0].State != "plan_changed" {
		t.Fatalf("plan edit not detected: %s", planReport)
	}
	if _, err := AcceptTask(taskRoot, filepath.Join(dir, "export")); err == nil {
		t.Fatal("accepted unverified candidate")
	}
}
