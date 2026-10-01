package portsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syncFixture(t *testing.T) (string, string, SyncOptions, string) {
	t.Helper()
	project, _ := updateFixture(t)
	if err := os.RemoveAll(filepath.Join(project, "migration/increment")); err != nil {
		t.Fatal(err)
	}
	testPut(t, project, ".gitignore", ".portsmith/\n.cache/\n")
	testPut(t, project, "internal/value/consumer.go", "package value\nfunc Consumer() int { return Value() }\n")
	wfGit(t, project, "add", ".")
	wfGit(t, project, "commit", "-qm", "sync target")
	repo := filepath.Join(t.TempDir(), "upstream")
	testPut(t, repo, "LICENSE", "fixture\n")
	testPut(t, repo, "src/value.ts", "export const value=41;\n")
	testPut(t, repo, "src/consumer.ts", "import {value} from './value.js'; export const consume=()=>value;\n")
	testPut(t, repo, "src/removed.ts", "export const old=true;\n")
	wfInitRepo(t, repo)
	wfGit(t, repo, "add", ".")
	wfGit(t, repo, "commit", "-qm", "old upstream")
	before := wfGit(t, repo, "rev-parse", "HEAD")
	c := SyncConfig{Version: 1, Repository: repo, Revision: before, Roots: []string{"src"}, Mappings: []SyncMapping{{Source: "src/value.ts", GoFiles: []string{"internal/value/value.go"}}, {Source: "src/consumer.ts", GoFiles: []string{"internal/value/consumer.go"}}, {Source: "src/removed.ts", GoFiles: []string{"internal/value/consumer.go"}}}}
	if err := AtomicJSON(filepath.Join(project, "migration/sync.json"), c); err != nil {
		t.Fatal(err)
	}
	wfGit(t, project, "add", "migration/sync.json")
	wfGit(t, project, "commit", "-qm", "sync configuration")
	testPut(t, repo, "src/value.ts", "export const value=42;\n")
	testPut(t, repo, "src/added.ts", "export const added=true;\n")
	if err := os.Remove(filepath.Join(repo, "src/removed.ts")); err != nil {
		t.Fatal(err)
	}
	wfGit(t, repo, "add", ".")
	wfGit(t, repo, "commit", "-qm", "new upstream")
	return project, repo, SyncOptions{Project: project, Source: repo, Upstream: wfGit(t, repo, "rev-parse", "HEAD")}, before
}
func TestSyncDraftTracksDiffDependenciesAndPreservesReview(t *testing.T) {
	project, repo, o, before := syncFixture(t)
	r, e := PrepareSync(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "needs-preparation" || r.Before != before || len(r.Changes) != 3 || len(r.Unmapped) != 1 || r.Unmapped[0] != "src/added.ts" {
		t.Fatalf("bad report: %+v", r)
	}
	if !containsString(r.AffectedSources, "src/consumer.ts") {
		t.Fatal("reverse dependency missing")
	}
	w, e := readJSON[moduleWorkflow](r.Plan, "workflow.json")
	if e != nil {
		t.Fatal(e)
	}
	if w.Updates == nil || len(w.Updates.Files) != 2 || w.Baseline == nil {
		t.Fatal("frozen update manifest missing")
	}
	p, e := readJSON[modulePlan](r.Plan, "plan.json")
	if e != nil {
		t.Fatal(e)
	}
	old := filepath.Join(project, p.Source, "sync-before", before, "src/removed.ts.txt")
	b, e := os.ReadFile(old)
	if e != nil || !strings.Contains(string(b), "old=true") {
		t.Fatal("deleted old source missing", e)
	}
	calls := 0
	out, e := Migrate(context.Background(), MigrationOptions{Plan: r.Plan, Check: true, Generate: func(context.Context, string, string) (RunReport, error) { calls++; return RunReport{}, nil }})
	if e != nil || wfDecode(t, out)["status"] != "needs-preparation" || calls != 0 {
		t.Fatal("draft executed", e, string(out))
	}
	testPut(t, r.Plan, "review.md", "Operator decisions\n")
	again, e := PrepareSync(context.Background(), o)
	if e != nil || again.Plan != r.Plan {
		t.Fatal(e)
	}
	if b, e := os.ReadFile(filepath.Join(r.Plan, "review.md")); e != nil || string(b) != "Operator decisions\n" {
		t.Fatal("review overwritten")
	}
	// Removing an ignored source cache must not lose a committed review.
	if e = os.RemoveAll(filepath.Join(project, ".cache/portsmith-upstream", o.Upstream)); e != nil {
		t.Fatal(e)
	}
	if _, e = PrepareSync(context.Background(), o); e != nil {
		t.Fatal("source restoration failed", e)
	}
	if b, e = os.ReadFile(old); e != nil || !strings.Contains(string(b), "old=true") {
		t.Fatal("old source not restored", e)
	}
	// A modified configuration cannot silently reuse the old plan.
	c, e := readJSON[SyncConfig](project, "migration/sync.json")
	if e != nil {
		t.Fatal(e)
	}
	c.Roots = append(c.Roots, "other")
	if e = AtomicJSON(filepath.Join(project, "migration/sync.json"), c); e != nil {
		t.Fatal(e)
	}
	if _, e = PrepareSync(context.Background(), o); e == nil {
		t.Fatal("config drift accepted")
	}
	_ = repo
}
func TestSyncRejectsTamperedSnapshotAndInvalidRevision(t *testing.T) {
	project, _, o, _ := syncFixture(t)
	r, e := PrepareSync(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.RemoveAll(r.Plan); e != nil {
		t.Fatal(e)
	}
	testPut(t, project, ".cache/portsmith-upstream/"+o.Upstream+"/src/value.ts", "export const value=999;\n")
	if _, e = PrepareSync(context.Background(), o); e == nil || !strings.Contains(e.Error(), "cache differs") {
		t.Fatal("cache tampering accepted", e)
	}
	o.Upstream = "main"
	if _, e = PrepareSync(context.Background(), o); e == nil {
		t.Fatal("mutable revision accepted")
	}
}
func prepareReviewedSync(t *testing.T, r SyncReport) {
	t.Helper()
	p, e := readJSON[modulePlan](r.Plan, "plan.json")
	if e != nil {
		t.Fatal(e)
	}
	outputs := append(p.Batches[0].Outputs, "internal/value/increment_test.go")
	p.Batches[0].Outputs = outputs
	if e = AtomicJSON(filepath.Join(r.Plan, "plan.json"), p); e != nil {
		t.Fatal(e)
	}
	testPut(t, r.Plan, "contract.md", "Return 42 from Value and Consumer; retain existing API. Added and removed TS exports are explicitly excluded from this test fixture.\n")
	testPut(t, r.Plan, "judge/internal/value/portsmith_judge_sync_test.go", "package value\nimport \"testing\"\nfunc TestPortsmithJudgeSync(t *testing.T){if Value()!=42||Consumer()!=42{t.Fatal(\"wrong result\")}}\n")
	w, e := readJSON[moduleWorkflow](r.Plan, "workflow.json")
	if e != nil {
		t.Fatal(e)
	}
	w.Batches["changes"] = batchConfig{Status: "ready", Steps: []stepDef{{ID: "port", Sources: p.Batches[0].Sources, Goal: "Update Value", Contract: "contract.md", Judge: "judge", Outputs: outputs, Tests: []string{"TestPortsmithJudgeSync"}}}}
	if e = AtomicJSON(filepath.Join(r.Plan, "workflow.json"), w); e != nil {
		t.Fatal(e)
	}
}
func TestSyncReviewedMigrationAdvancesOnlyAfterAcceptance(t *testing.T) {
	project, _, o, before := syncFixture(t)
	r, e := PrepareSync(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	prepareReviewedSync(t, r)
	if e = AtomicJSON(filepath.Join(r.Plan, "new-mappings.json"), []SyncMapping{{Source: "src/value.ts", GoFiles: []string{"internal/value/value.go", "internal/value/consumer.go"}}}); e != nil {
		t.Fatal(e)
	}
	var log bytes.Buffer
	if code := Main(context.Background(), []string{"sync", "--project", project, "--source", o.Source, "--upstream", o.Upstream, "--check"}, &log, &log); code != 0 {
		t.Fatal(code, log.String())
	}
	c, e := readJSON[SyncConfig](project, "migration/sync.json")
	if e != nil || c.Revision != before {
		t.Fatal("check advanced revision", e)
	}
	_, e = Migrate(context.Background(), MigrationOptions{Plan: r.Plan, Commit: true, MaxAttempts: 1, Generate: updateGenerator(t)})
	if e != nil {
		t.Fatal(e)
	}
	if e = advanceSync(context.Background(), &cliOptions{}, r); e != nil {
		t.Fatal(e)
	}
	c, e = readJSON[SyncConfig](project, "migration/sync.json")
	if e != nil || c.Revision != o.Upstream {
		t.Fatal("revision not advanced", e)
	}
	head := wfGit(t, project, "rev-parse", "HEAD")
	again, e := PrepareSync(context.Background(), o)
	if e != nil || again.Status != "no_changes" || wfGit(t, project, "rev-parse", "HEAD") != head {
		t.Fatal("sync not idempotent", e)
	}
	if len(c.Mappings[2].GoFiles) != 2 {
		t.Fatal("reviewed ownership not advanced")
	}
	if len(c.Mappings) != 3 {
		t.Fatal("excluded source was silently mapped")
	}
	if wfGit(t, project, "status", "--porcelain") != "" {
		t.Fatal("dirty integration")
	}
}
func TestSyncMetadataCommitRecovery(t *testing.T) {
	for _, mode := range []string{"before-write", "before-commit", "after-commit", "tamper"} {
		t.Run(mode, func(t *testing.T) {
			project, _, _, _ := syncFixture(t)
			old, e := readCheckedBytes(project, "migration/sync.json")
			if e != nil {
				t.Fatal(e)
			}
			var c SyncConfig
			if e = json.Unmarshal(old, &c); e != nil {
				t.Fatal(e)
			}
			c.Revision = strings.Repeat("a", 40)
			b, e := json.MarshalIndent(c, "", "  ")
			if e != nil {
				t.Fatal(e)
			}
			b = append(b, '\n')
			pending := syncAdvance{Config: "migration/sync.json", Before: Hash(old), After: File{Name: "migration/sync.json", Data: b, SHA256: Hash(b)}, Head: wfGit(t, project, "rev-parse", "HEAD"), Message: "chore: sync metadata"}
			if e = os.MkdirAll(filepath.Join(project, ".portsmith"), 0755); e != nil {
				t.Fatal(e)
			}
			if e = AtomicJSON(filepath.Join(project, ".portsmith/sync-advance.json"), pending); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "before-commit", "after-commit":
				if e = replaceProjectFile(project, pending.After); e != nil {
					t.Fatal(e)
				}
			case "tamper":
				testPut(t, project, "migration/sync.json", "edited")
			}
			if mode == "after-commit" {
				if _, e = commitFiles(context.Background(), project, []string{pending.Config}, pending.Message); e != nil {
					t.Fatal(e)
				}
			}
			e = recoverSyncAdvance(context.Background(), project)
			if mode == "tamper" {
				if e == nil {
					t.Fatal("overwrote user edits")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if wfGit(t, project, "status", "--porcelain") != "" {
				t.Fatal("dirty metadata recovery")
			}
			head := wfGit(t, project, "rev-parse", "HEAD")
			if e = recoverSyncAdvance(context.Background(), project); e != nil || head != wfGit(t, project, "rev-parse", "HEAD") {
				t.Fatal("non-idempotent recovery", e)
			}
		})
	}
}

func TestInitSyncImportsReviewedReferencesAndFineMappings(t *testing.T) {
	project, _ := updateFixture(t)
	testPut(t, project, "internal/value/consumer.go", "package value\n")
	rev := strings.Repeat("b", 40)
	if e := AtomicJSON(filepath.Join(project, "migration/upstream.json"), map[string]string{"repository": "https://example.com/source.git", "commit": rev}); e != nil {
		t.Fatal(e)
	}
	p := modulePlan{Version: 2, Revision: rev, Batches: []batchDef{{Sources: []string{"packages/ai/src/types.ts"}, References: []string{"packages/coding-agent/src/utils/abort.ts"}, Outputs: []string{"internal/value/value.go", "internal/value/consumer.go"}}}}
	if e := AtomicJSON(filepath.Join(project, "migration/plan.json"), p); e != nil {
		t.Fatal(e)
	}
	testPut(t, project, "packages/test/source_map.json", `[{"source":"packages/ai/src/types.ts","goFile":"internal/value/value.go"}]`)
	config := filepath.Join(project, "migration/sync.json")
	if e := InitSync(project, config); e != nil {
		t.Fatal(e)
	}
	c, e := readJSON[SyncConfig](project, "migration/sync.json")
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Mappings) != 2 || len(c.Mappings[0].GoFiles) != 1 || len(c.Mappings[1].GoFiles) != 2 {
		t.Fatalf("wrong imported ownership: %+v", c)
	}
	if e := InitSync(project, config); e == nil {
		t.Fatal("overwrote config")
	}
}

func TestSyncInvalidOwnershipRejectedBeforeModelConfiguration(t *testing.T) {
	project, _, o, _ := syncFixture(t)
	r, e := PrepareSync(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	prepareReviewedSync(t, r)
	if e = AtomicJSON(filepath.Join(r.Plan, "new-mappings.json"), []SyncMapping{{Source: "src/added.ts", GoFiles: []string{"internal/not-owned.go"}}}); e != nil {
		t.Fatal(e)
	}
	var out, errOut bytes.Buffer
	code := Main(context.Background(), []string{"sync", "--project", project, "--source", o.Source, "--upstream", o.Upstream, "--commit", "--env-file", filepath.Join(project, "absent.env")}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "not a reviewed Go output") {
		t.Fatal("invalid mapping reached model configuration", code, errOut.String())
	}
	if wfGit(t, project, "status", "--porcelain") != "?? migration/sync/" {
		t.Fatal("invalid ownership modified target history")
	}
}

func archiveAttributeFixture(t *testing.T) (string, string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	testPut(t, repo, ".gitattributes", "*.bat text eol=crlf\nbanner.txt export-subst\nhidden.txt export-ignore\n")
	testPut(t, repo, "pi-test.bat", "@echo off\necho fixture\n")
	testPut(t, repo, "banner.txt", "commit=$Format:%H$\n")
	testPut(t, repo, "hidden.txt", "tracked and required\n")
	wfInitRepo(t, repo)
	wfGit(t, repo, "add", ".")
	wfGit(t, repo, "commit", "-qm", "archive attribute fixture")
	return repo, wfGit(t, repo, "rev-parse", "HEAD")
}
func TestSyncExportPreservesRawBlobsDespiteArchiveAttributes(t *testing.T) {
	repo, revision := archiveAttributeFixture(t)
	destination := filepath.Join(t.TempDir(), "snapshot")
	if e := exportRevision(context.Background(), repo, revision, destination); e != nil {
		t.Fatal(e)
	}
	if e := validateSnapshot(context.Background(), repo, revision, destination); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"pi-test.bat", "banner.txt", "hidden.txt"} {
		want, e := gitBlob(context.Background(), repo, revision, name)
		if e != nil {
			t.Fatal(e)
		}
		got, e := readCheckedBytes(destination, name)
		if e != nil || !bytes.Equal(got, want) {
			t.Fatal("export transformed raw blob", name, e)
		}
	}
}
func TestSyncRepairsOnlyVerifiedLegacyArchiveBytes(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "tampered"}[tamper], func(t *testing.T) {
			repo, revision := archiveAttributeFixture(t)
			project := t.TempDir()
			snapshot := filepath.Join(project, ".cache/portsmith-upstream", revision)
			for _, name := range []string{".gitattributes", "pi-test.bat", "banner.txt", "hidden.txt"} {
				b, present, e := archiveFile(context.Background(), repo, revision, name)
				if e != nil {
					t.Fatal(e)
				}
				if present {
					testPut(t, snapshot, name, string(b))
				}
			}
			raw, e := gitBlob(context.Background(), repo, revision, "pi-test.bat")
			if e != nil {
				t.Fatal(e)
			}
			archived, e := readCheckedBytes(snapshot, "pi-test.bat")
			if e != nil {
				t.Fatal(e)
			}
			if bytes.Equal(raw, archived) || !bytes.Contains(archived, []byte("\r\n")) {
				t.Fatal("fixture did not reproduce archive EOL conversion")
			}
			testPut(t, snapshot, "sync-before/old/source.ts.txt", "frozen previous source\n")
			if tamper {
				testPut(t, snapshot, "banner.txt", "user edited this cache\n")
			}
			p, e := syncSnapshot(context.Background(), project, repo, revision)
			if tamper {
				if e == nil {
					t.Fatal("overwrote edited archive cache")
				}
				b, _ := readCheckedBytes(snapshot, "banner.txt")
				if string(b) != "user edited this cache\n" {
					t.Fatal("edit lost")
				}
				b, _ = readCheckedBytes(snapshot, "pi-test.bat")
				if !bytes.Equal(b, archived) {
					t.Fatal("partial repair before discovering edit")
				}
				return
			}
			if e != nil || p != snapshot {
				t.Fatal("legacy cache not repaired", e)
			}
			if e = validateSnapshot(context.Background(), repo, revision, snapshot); e != nil {
				t.Fatal(e)
			}
			b, e := readCheckedBytes(snapshot, "sync-before/old/source.ts.txt")
			if e != nil || string(b) != "frozen previous source\n" {
				t.Fatal("frozen source lost", e)
			}
			if _, e = syncSnapshot(context.Background(), project, repo, revision); e != nil {
				t.Fatal("repair was not idempotent", e)
			}
		})
	}
}
