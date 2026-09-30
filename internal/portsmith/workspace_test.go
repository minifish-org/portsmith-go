package portsmith

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// workspaceFixture builds a source tree with a LICENSE and one reference file.
func workspaceFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	testPut(t, source, "LICENSE", "fixture license\n")
	testPut(t, source, "value.ts", "export function Value(){ return 42; }\n")
	return dir, source
}

func workspaceMod(t *testing.T, dir string) string {
	t.Helper()
	mod := filepath.Join(dir, "go.mod")
	testPut(t, dir, "go.mod", "module example.com/candidate\n\ngo 1.24\n")
	return mod
}

func TestPrepareTaskSnapshotsAndLoadTask(t *testing.T) {
	dir, source := workspaceFixture(t)
	root, err := PrepareTask(context.Background(), PrepareOptions{
		Source:   source,
		Out:      filepath.Join(dir, "task"),
		Files:    []string{"value.ts", "value.ts"},
		Revision: "rev-1",
		Goal:     "Port Value",
		GoMod:    workspaceMod(t, dir),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"references/value.ts", "references/LICENSE", "RULEBOOK.md", "candidate/go.mod", "candidate/LICENSE", "task.json"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	raw, err := LoadTask(root)
	if err != nil {
		t.Fatal(err)
	}
	var task PortTask
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	if task.Version != 1 || task.Revision != "rev-1" || task.Goal != "Port Value" {
		t.Fatalf("unexpected task: %+v", task)
	}
	if len(task.Files) != 2 || task.Files[0].Path != "value.ts" || task.Files[1].Path != "LICENSE" {
		t.Fatalf("unexpected reference records: %+v", task.Files)
	}
	if task.Files[0].Bytes == 0 || task.Files[0].SHA256 == "" {
		t.Fatalf("reference not frozen: %+v", task.Files[0])
	}
	if len(task.DependsOn) != 0 || task.DependsOn == nil {
		t.Fatalf("dependsOn must be an empty array: %#v", task.DependsOn)
	}
}

func TestPrepareTaskRefusesExistingRoot(t *testing.T) {
	dir, source := workspaceFixture(t)
	out := filepath.Join(dir, "task")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareTask(context.Background(), PrepareOptions{Source: source, Out: out, Files: []string{"value.ts"}, Goal: "x"}); err == nil {
		t.Fatal("prepare overwrote an existing task directory")
	}
}

func TestPrepareTaskRejectsLocalReplaceAndBadGoal(t *testing.T) {
	dir, source := workspaceFixture(t)
	mod := filepath.Join(dir, "go.mod")
	testPut(t, dir, "go.mod", "module example.com/test\n\nreplace example.com/secret => ../secret\n")
	if _, err := PrepareTask(context.Background(), PrepareOptions{Source: source, Out: filepath.Join(dir, "a"), Files: []string{"value.ts"}, Goal: "x", GoMod: mod}); err == nil {
		t.Fatal("local replace directive accepted")
	}
	if _, err := PrepareTask(context.Background(), PrepareOptions{Source: source, Out: filepath.Join(dir, "b"), Files: []string{"value.ts"}, Goal: "   "}); err == nil {
		t.Fatal("blank goal accepted")
	}
	if _, err := PrepareTask(context.Background(), PrepareOptions{Source: source, Out: filepath.Join(dir, "c"), Goal: "x"}); err == nil {
		t.Fatal("empty reference list accepted")
	}
}

func TestWriteCandidateManifestAndControlFiles(t *testing.T) {
	dir, source := workspaceFixture(t)
	root, err := PrepareTask(context.Background(), PrepareOptions{
		Source:             source,
		Out:                filepath.Join(dir, "task"),
		Files:              []string{"value.ts"},
		Goal:               "x",
		ModuleTask:         true,
		WritableFiles:      []string{"value.go", "value_test.go", "notes.txt", "fragment.go"},
		RequiredJudgeTests: []string{"TestPortsmithJudgeValue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value.go", []byte("package port\nfunc Value() int { return 42 }\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value_test.go", []byte("package port\nfunc helper() {}\n")); err != nil {
		t.Fatal(err)
	}
	// module asset allowed by the writable manifest
	if err := WriteCandidate(root, "notes.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"other.go", "../escape.go", "go.mod", "go.sum", "LICENSE", ".hidden.go", "portsmith_judge_x.go", "sub/port_oracle.go"} {
		if err := WriteCandidate(root, name, []byte("package port\n")); err == nil {
			t.Fatalf("accepted forbidden candidate %q", name)
		}
	}
	if err := WriteCandidate(root, "fragment.go", []byte("func Value() int { return 42 }\n")); err == nil {
		t.Fatal("fragment without package declaration accepted")
	}
	// A manifest restricts writes to exactly the listed Go files.
	if err := WriteCandidate(root, "notlisted.go", []byte("package port\n")); err == nil {
		t.Fatal("file outside the writable manifest accepted")
	}
}

func TestEditCandidateUniqueReplacement(t *testing.T) {
	dir, source := workspaceFixture(t)
	root, err := PrepareTask(context.Background(), PrepareOptions{Source: source, Out: filepath.Join(dir, "task"), Files: []string{"value.ts"}, Goal: "x"})
	if err != nil {
		t.Fatal(err)
	}
	original := "package port\nconst Answer = 41\n// keep this\n"
	if err := WriteCandidate(root, "unit.go", []byte(original)); err != nil {
		t.Fatal(err)
	}
	if err := EditCandidate(root, "unit.go", "Answer = 41", "Answer = 42"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "candidate", "unit.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "package port\nconst Answer = 42\n// keep this\n" {
		t.Fatalf("edit corrupted file: %q", data)
	}
	if err := EditCandidate(root, "unit.go", "missing", "x"); err == nil {
		t.Fatal("missing replacement accepted")
	}
	if err := EditCandidate(root, "unit.go", " ", "x"); err == nil {
		t.Fatal("ambiguous replacement accepted")
	}
	if err := EditCandidate(root, "unit.go", "", "x"); err == nil {
		t.Fatal("empty oldText accepted")
	}
	if err := EditCandidate(root, "go.mod", "1.24", "1.25"); err == nil {
		t.Fatal("dependency edit accepted")
	}
}

func TestCandidateFilesDeterministic(t *testing.T) {
	dir, source := workspaceFixture(t)
	sum := filepath.Join(dir, "go.sum")
	testPut(t, dir, "go.sum", "")
	root, err := PrepareTask(context.Background(), PrepareOptions{
		Source: source, Out: filepath.Join(dir, "task"), Files: []string{"value.ts"}, Goal: "x",
		GoMod: workspaceMod(t, dir), GoSum: sum,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "b.go", []byte("package port\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "a.go", []byte("package port\n")); err != nil {
		t.Fatal(err)
	}
	files, err := CandidateFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Name)
	}
	for _, want := range []string{"go.mod", "go.sum", "LICENSE", "a.go", "b.go"} {
		if !containsString(names, want) {
			t.Fatalf("candidate snapshot missing %s: %v", want, names)
		}
	}
	again, err := CandidateFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := range files {
		if files[i].Name != again[i].Name || files[i].SHA256 != again[i].SHA256 {
			t.Fatalf("candidate order not deterministic: %v vs %v", names, again)
		}
	}
}

func TestFingerprintDetectsMutationAndUnfrozenGoSum(t *testing.T) {
	dir, source := workspaceFixture(t)
	root, err := PrepareTask(context.Background(), PrepareOptions{Source: source, Out: filepath.Join(dir, "task"), Files: []string{"value.ts"}, Goal: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value.go", []byte("package port\nfunc Value() int { return 42 }\n")); err != nil {
		t.Fatal(err)
	}
	before, err := Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if before != again {
		t.Fatal("fingerprint not stable")
	}
	if err := EditCandidate(root, "value.go", "return 42", "return 43"); err != nil {
		t.Fatal(err)
	}
	after, err := Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("fingerprint ignored candidate mutation")
	}
	// An undeclared go.sum is rejected before hashing.
	if err := os.WriteFile(filepath.Join(root, "candidate", "go.sum"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Fingerprint(root); err == nil {
		t.Fatal("unfrozen go.sum accepted")
	}
}

func TestLoadTaskDetectsTampering(t *testing.T) {
	dir, source := workspaceFixture(t)
	prepare := func(out string) string {
		t.Helper()
		root, err := PrepareTask(context.Background(), PrepareOptions{
			Source: source,
			Out:    filepath.Join(dir, out),
			Files:  []string{"value.ts"},
			Goal:   "x",
			GoMod:  workspaceMod(t, dir),
			JudgeFiles: []File{
				{Name: "judge_test.go", Data: []byte("package port\nimport \"testing\"\nfunc TestPortsmithJudgeValue(t *testing.T){}\n")},
			},
			Seed: []File{
				{Name: "seed.go", Data: []byte("package port\n// seed\n")},
			},
			WritableFiles:      []string{"value.go"},
			RequiredJudgeTests: []string{"TestPortsmithJudgeValue"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return root
	}

	root := prepare("a")
	raw, err := LoadTask(root)
	if err != nil {
		t.Fatal(err)
	}
	var task PortTask
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	if got := stringList(task.RequiredJudgeTests); len(got) != 1 || got[0] != "TestPortsmithJudgeValue" {
		t.Fatalf("required judge tests lost: %+v", task.RequiredJudgeTests)
	}
	if seeds := namedDigestList(task.SeedFiles); len(seeds) != 1 || seeds[0].Name != "seed.go" {
		t.Fatalf("seed files lost: %+v", task.SeedFiles)
	}

	root = prepare("b")
	testPut(t, root, "references/value.ts", "modified\n")
	if _, err := LoadTask(root); err == nil {
		t.Fatal("reference tampering accepted")
	}

	root = prepare("c")
	testPut(t, root, "candidate/go.mod", "module changed\n\ngo 1.24\n")
	if _, err := LoadTask(root); err == nil {
		t.Fatal("manifest tampering accepted")
	}

	root = prepare("d")
	testPut(t, root, "judge/judge_test.go", "// changed\n")
	if _, err := LoadTask(root); err == nil {
		t.Fatal("judge tampering accepted")
	}

	root = prepare("e")
	testPut(t, root, "candidate/seed.go", "package port\n// changed\n")
	if _, err := LoadTask(root); err == nil {
		t.Fatal("seed tampering accepted")
	}
}

func TestSeedAndInitialRules(t *testing.T) {
	dir, source := workspaceFixture(t)
	seed := []File{{Name: "seed.go", Data: []byte("package port\n")}}

	if _, err := PrepareTask(context.Background(), PrepareOptions{
		Source: source, Out: filepath.Join(dir, "a"), Files: []string{"value.ts"}, Goal: "x",
		Seed: seed, WritableFiles: []string{"seed.go"},
	}); err == nil {
		t.Fatal("seed file also writable accepted")
	}
	if _, err := PrepareTask(context.Background(), PrepareOptions{
		Source: source, Out: filepath.Join(dir, "b"), Files: []string{"value.ts"}, Goal: "x",
		Seed: seed, Initial: seed, ModuleTask: true, WritableFiles: []string{"seed.go"},
	}); err == nil {
		t.Fatal("initial overwriting a seed accepted")
	}
	if _, err := PrepareTask(context.Background(), PrepareOptions{
		Source: source, Out: filepath.Join(dir, "c"), Files: []string{"value.ts"}, Goal: "x",
		Initial: []File{{Name: "out.go", Data: []byte("package port\n")}}, WritableFiles: []string{"out.go"},
	}); err == nil {
		t.Fatal("initial without moduleTask accepted")
	}
	root, err := PrepareTask(context.Background(), PrepareOptions{
		Source: source, Out: filepath.Join(dir, "d"), Files: []string{"value.ts"}, Goal: "x",
		ModuleTask: true, WritableFiles: []string{"out.go"},
		Seed: seed, Initial: []File{{Name: "out.go", Data: []byte("package port\n// initial\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A seed is never writable, even when no manifest restricts writes.
	seedOnly, err := PrepareTask(context.Background(), PrepareOptions{
		Source: source, Out: filepath.Join(dir, "e"), Files: []string{"value.ts"}, Goal: "x",
		Seed: seed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(seedOnly, "seed.go", []byte("package port\n// overwrite\n")); err == nil {
		t.Fatal("seed code modify accepted")
	}
	// Initial files are prepared outputs; they remain editable in the manifest.
	if err := WriteCandidate(root, "out.go", []byte("package port\n// edited\n")); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyWritableManifestPreserved(t *testing.T) {
	dir, source := workspaceFixture(t)
	root, err := PrepareTask(context.Background(), PrepareOptions{
		Source: source, Out: filepath.Join(dir, "task"), Files: []string{"value.ts"}, Goal: "x",
		ModuleTask: true, WritableFiles: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("task.json is not valid JSON: %s", data)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["writableFiles"]; !ok {
		t.Fatalf("explicit empty writable manifest was dropped: %s", data)
	}
	if err := WriteCandidate(root, "value.go", []byte("package port\n")); err == nil {
		t.Fatal("empty writable manifest allowed a write")
	}
}

func TestPrepareTaskRaceAndRequiredJudgeSurvive(t *testing.T) {
	dir, source := workspaceFixture(t)
	root, err := PrepareTask(context.Background(), PrepareOptions{
		Source:             source,
		Out:                filepath.Join(dir, "task"),
		Files:              []string{"value.ts"},
		Goal:               "x",
		Race:               true,
		ModuleTask:         true,
		RequiredJudgeTests: []string{"TestPortsmithJudgeA", "TestPortsmithJudgeB"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["race"] != true || decoded["moduleTask"] != true {
		t.Fatalf("race/moduleTask lost: %v", decoded)
	}
	tests, ok := decoded["requiredJudgeTests"].([]any)
	if !ok || len(tests) != 2 {
		t.Fatalf("requiredJudgeTests lost: %v", decoded["requiredJudgeTests"])
	}
	if _, ok := decoded["goSumSha256"]; ok {
		t.Fatal("absent go.sum must not create goSumSha256")
	}
}
