package portsmith

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const rollbackValueOriginal = "package value\nfunc Value() int {return 41}\n"

func integrationUpdateFixture(t *testing.T) (string, string, moduleState) {
	t.Helper()
	project, plan := updateFixture(t)
	testPut(t, project, "legacy_test.go", `package legacy
import ("os"; "strings"; "testing")
func TestPortsmithJudgeHistoricalUpdate(t *testing.T) {
 b, err := os.ReadFile("internal/value/value.go")
 if err != nil {t.Fatal(err)}
 if !strings.Contains(string(b), "// integration-ready") {t.Fatal("historical update integration marker missing")}
}
`)
	wfGit(t, project, "add", "legacy_test.go")
	wfGit(t, project, "commit", "-qm", "historical update gate")
	_, err := Migrate(context.Background(), MigrationOptions{Plan: plan, Commit: true, MaxAttempts: 1, Generate: updateGenerator(t)})
	if err == nil || !strings.Contains(err.Error(), "generation/repair attempts") {
		t.Fatalf("expected clean repair checkpoint at the configured budget: %v", err)
	}
	state, err := readJSON[moduleState](project, ".portsmith/increment/modules.json")
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending != nil || state.Repair == nil || len(state.Steps) != 0 {
		t.Fatalf("integration update was not reopened: %+v", state)
	}
	b, err := os.ReadFile(filepath.Join(project, "internal/value/value.go"))
	if err != nil || string(b) != rollbackValueOriginal {
		t.Fatalf("original update bytes were not restored: %s %v", b, err)
	}
	if status := wfGit(t, project, "status", "--porcelain"); status != "" {
		t.Fatalf("failed update left target dirty: %s", status)
	}
	return project, plan, state
}

// Recreate the saved rollback phase, using the actual frozen staging and
// checkpoint written by the failed run. This simulates a process disappearing
// after the rollback intent was saved, including mixed restored/applied files.
func integrationPendingRollback(t *testing.T, project string, state moduleState) moduleState {
	t.Helper()
	stagingRel := ".portsmith/increment/runs/increment-integration"
	staging := filepath.Join(project, filepath.FromSlash(stagingRel))
	files, err := snapshotFiles(staging, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := readJSON[struct {
		Steps []moduleDone `json:"steps"`
	}](staging, "migration/results/increment.json")
	if err != nil || len(receipt.Steps) != 1 {
		t.Fatalf("missing real final checkpoint: %+v %v", receipt, err)
	}
	last := receipt.Steps[0]
	state.Steps = receipt.Steps
	state.Pending = &modulePending{
		Module: "increment", Base: wfGit(t, project, "rev-parse", "HEAD"),
		Files: entriesOf(files), Task: last.Task, Fingerprint: last.Fingerprint,
		Staging: stagingRel, Before: []NamedDigest{{Name: "internal/value/value.go", SHA256: Hash([]byte(rollbackValueOriginal))}},
		Repair: state.Repair,
	}
	state.Repair = nil
	if err := AtomicJSON(filepath.Join(project, ".portsmith/increment/modules.json"), state); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Name == "internal/value/value.go" {
			if err := replaceProjectFile(project, file); err != nil {
				t.Fatal(err)
			}
		} else if err := copyFiles(project, []File{file}); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func TestWorkflowV2IntegrationRollbackResumesExistingFiles(t *testing.T) {
	for _, mode := range []string{"repair", "partial-rollback", "staged"} {
		t.Run(mode, func(t *testing.T) {
			project, plan, state := integrationUpdateFixture(t)
			if mode != "repair" {
				state = integrationPendingRollback(t, project, state)
				if mode == "partial-rollback" {
					testPut(t, project, "internal/value/value.go", rollbackValueOriginal)
					if err := os.Remove(filepath.Join(project, "internal/value/increment_test.go")); err != nil {
						t.Fatal(err)
					}
				} else {
					names := []string{"add", "--"}
					for _, file := range state.Pending.Files {
						names = append(names, file.Name)
					}
					wfGit(t, project, names...)
				}
			}
			calls := 0
			result, err := Migrate(context.Background(), MigrationOptions{Plan: plan, Commit: true, MaxAttempts: 1,
				Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
					calls++
					if !strings.Contains(feedback, "historical update integration marker missing") {
						t.Fatalf("missing integration diagnostics: %s", feedback)
					}
					b, err := os.ReadFile(filepath.Join(project, "internal/value/value.go"))
					if err != nil || string(b) != rollbackValueOriginal || wfGit(t, project, "status", "--porcelain") != "" {
						t.Fatalf("repair ran before clean original restoration: %s %v", b, err)
					}
					return RunReport{Status: "candidate_ready"}, WriteCandidate(root, "internal/value/value.go",
						[]byte("package value\n// integration-ready\nfunc Value() int {return 42}\n"))
				}})
			if err != nil || wfDecode(t, result)["status"] != "complete" || calls != 1 {
				t.Fatalf("interrupted integration repair did not finish: %s %v calls=%d", result, err, calls)
			}
			if len(wfFeatCommits(t, project)) != 1 || wfGit(t, project, "status", "--porcelain") != "" {
				t.Fatal("repair duplicated a commit or left dirty files")
			}
		})
	}
}

func TestWorkflowV2IntegrationRollbackRefusesExternalChanges(t *testing.T) {
	for _, mode := range []string{"owned-file", "unrelated-file", "index-bytes", "index-mode"} {
		t.Run(mode, func(t *testing.T) {
			project, plan, state := integrationUpdateFixture(t)
			integrationPendingRollback(t, project, state)
			switch mode {
			case "owned-file":
				testPut(t, project, "internal/value/increment_test.go", "package value\n// user edit\n")
			case "unrelated-file":
				testPut(t, project, "user.txt", "keep user work\n")
			case "index-bytes":
				testPut(t, project, "internal/value/value.go", "package value\nfunc Value() int {return 99}\n")
				wfGit(t, project, "add", "internal/value/value.go")
				testPut(t, project, "internal/value/value.go", "package value\nfunc Value() int {return 42}\n")
			case "index-mode":
				wfGit(t, project, "add", "internal/value/value.go")
				wfGit(t, project, "update-index", "--chmod=+x", "internal/value/value.go")
			}
			before := wfGit(t, project, "status", "--porcelain")
			_, err := Migrate(context.Background(), MigrationOptions{Plan: plan, Commit: true, Generate: wfNoModel})
			if err == nil {
				t.Fatal("external change was accepted for rollback")
			}
			if wfGit(t, project, "status", "--porcelain") != before {
				t.Fatal("rollback modified target before validating every owned and index path")
			}
			b, err := os.ReadFile(filepath.Join(project, "internal/value/value.go"))
			if err != nil || !strings.Contains(string(b), "42") {
				t.Fatalf("rollback partially restored files before rejecting user changes: %s %v", b, err)
			}
			if len(wfFeatCommits(t, project)) != 0 {
				t.Fatal("rejected rollback created a commit")
			}
		})
	}
}
