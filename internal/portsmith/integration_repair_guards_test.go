package portsmith

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowV2IntegrationRepairRejectsReportChangedDuringGeneration(t *testing.T) {
	f := integrationRepairFixture(t)
	second := 0
	_, err := Migrate(context.Background(), MigrationOptions{
		Plan: f.plan, Commit: true,
		Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
			if strings.HasSuffix(root, "/Second") {
				second++
				if second == 2 {
					report, _ := integrationRepairAssertFeedback(t, f, root, feedback)
					if err := integrationRepairPutMarker(root); err != nil {
						return RunReport{}, err
					}
					// A successful candidate repair cannot rewrite the evidence that
					// triggered it, even through an unrestricted generator callback.
					if err := os.WriteFile(report, []byte("{\"code\":0,\"log\":\"altered during generation\"}\n"), 0o600); err != nil {
						return RunReport{}, err
					}
					return RunReport{Status: "candidate_ready"}, nil
				}
				if second > 2 {
					t.Fatal("evidence drift reached another generation attempt")
				}
			}
			return f.generate(root, false)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Integration failure report was modified") || second != 2 {
		t.Fatalf("report drift during generation was accepted: err=%v calls=%d", err, second)
	}
	integrationRepairAssertClean(t, f)
	state := integrationRepairState(t, f)
	if state.Repair == nil || state.Pending != nil || len(state.Steps) != 1 || len(state.Modules) != 0 {
		t.Fatalf("evidence drift published a revised checkpoint: %+v", state)
	}
}

func TestWorkflowV2IntegrationRepairRejectsFailureDirectorySymlink(t *testing.T) {
	f := integrationRepairFixture(t)
	outside := t.TempDir()
	// Probe symlink support before starting a migration so unsupported hosts
	// skip without leaving a partially executed fixture.
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Symlink(outside, probe); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(outside)
	if err != nil || len(before) != 0 {
		t.Fatalf("outside fixture is not empty: %v entries=%d", err, len(before))
	}
	calls := 0
	_, err = Migrate(context.Background(), MigrationOptions{
		Plan: f.plan, Commit: true,
		Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
			calls++
			if calls > 2 {
				t.Fatal("unsafe report directory reached agent repair")
			}
			result, err := f.generate(root, false)
			if err != nil {
				return result, err
			}
			if strings.HasSuffix(root, "/Second") {
				if err := os.Symlink(outside, filepath.Join(root, "integration-failures")); err != nil {
					return RunReport{}, err
				}
			}
			return result, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Integration failure directory must be a regular directory") || calls != 2 {
		t.Fatalf("unsafe failure directory was followed: err=%v calls=%d", err, calls)
	}
	after, err := os.ReadDir(outside)
	if err != nil || len(after) != 0 {
		t.Fatalf("failure archive escaped the task: %v entries=%d", err, len(after))
	}
	if commits := wfFeatCommits(t, f.project); len(commits) != 0 {
		t.Fatalf("unsafe archive directory allowed a module commit: %v", commits)
	}
	state := integrationRepairState(t, f)
	if state.Pending == nil || state.Pending.Repair != nil || state.Repair != nil || len(state.Modules) != 0 {
		t.Fatalf("unsafe archive directory discarded the pending transaction: %+v", state)
	}
}

func TestWorkflowV2IntegrationRepairRejectsUnrelatedMutationAfterPassingTests(t *testing.T) {
	f := integrationRepairFixture(t)
	testPut(t, f.project, "unrelated.txt", "original user data\n")
	legacy, err := os.ReadFile(filepath.Join(f.project, "legacy_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	// The historical gate first fails normally. After the agent repairs it,
	// every test passes but a test changes a file outside the transaction.
	legacy = append(legacy, []byte(`
func TestIntegrationMutatesUnrelatedOnPass(t *testing.T) {
 data, err := os.ReadFile("alpha/helper.go")
 if err != nil { t.Fatal(err) }
 if !strings.Contains(string(data), "// integration-ready") { return }
 if err := os.WriteFile("unrelated.txt", []byte("changed during passing integration\n"), 0644); err != nil { t.Fatal(err) }
}
`)...)
	testPut(t, f.project, "legacy_test.go", string(legacy))
	wfGit(t, f.project, "add", "unrelated.txt", "legacy_test.go")
	wfGit(t, f.project, "commit", "-qm", "integration mutation regression")
	second := 0
	_, err = Migrate(context.Background(), MigrationOptions{
		Plan: f.plan, Commit: true,
		Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
			if strings.HasSuffix(root, "/Second") {
				second++
				if second == 2 {
					integrationRepairAssertFeedback(t, f, root, feedback)
					return RunReport{Status: "candidate_ready"}, integrationRepairPutMarker(root)
				}
				if second > 2 {
					t.Fatal("unrelated mutation reached another generation attempt")
				}
			}
			return f.generate(root, false)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Integration refuses unrelated working-tree changes: unrelated.txt") || second != 2 {
		t.Fatalf("passing integration committed unrelated drift: err=%v calls=%d", err, second)
	}
	if commits := wfFeatCommits(t, f.project); len(commits) != 0 {
		t.Fatalf("passing tests created a premature module commit: %v", commits)
	}
	data, err := os.ReadFile(filepath.Join(f.project, "unrelated.txt"))
	if err != nil || string(data) != "changed during passing integration\n" {
		t.Fatalf("unrelated changed bytes were discarded: %v %q", err, data)
	}
	state := integrationRepairState(t, f)
	if state.Pending == nil || len(state.Modules) != 0 || len(state.Steps) != 2 {
		t.Fatalf("passing-test drift discarded resumable transaction: %+v", state)
	}
	result, err := readJSON[ProcessResult](f.project, state.Pending.Task+"/integration-tests.json")
	if err != nil || !succeeded(result) {
		t.Fatalf("fixture did not reproduce mutation after passing tests: %v %+v", err, result)
	}
}

func TestWorkflowV2IntegrationRepairRejectsCandidateMutationAfterPassingTests(t *testing.T) {
	f := integrationRepairFixture(t)
	legacy, err := os.ReadFile(filepath.Join(f.project, "legacy_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	legacy = append(legacy, []byte(`
func TestIntegrationMutatesCandidateOnPass(t *testing.T) {
 data, err := os.ReadFile("alpha/helper.go")
 if err != nil { t.Fatal(err) }
 if !strings.Contains(string(data), "// integration-ready") { return }
 candidate := ".portsmith/runs/alpha/foundation/Second/candidate/alpha/helper.go"
 data, err = os.ReadFile(candidate)
 if err != nil { t.Fatal(err) }
 data = append(data, []byte("// candidate changed during passing integration\n")...)
 if err := os.WriteFile(candidate, data, 0644); err != nil { t.Fatal(err) }
}
`)...)
	testPut(t, f.project, "legacy_test.go", string(legacy))
	wfGit(t, f.project, "add", "legacy_test.go")
	wfGit(t, f.project, "commit", "-qm", "ignored candidate mutation regression")
	second := 0
	_, err = Migrate(context.Background(), MigrationOptions{
		Plan: f.plan, Commit: true,
		Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
			if strings.HasSuffix(root, "/Second") {
				second++
				if second == 2 {
					integrationRepairAssertFeedback(t, f, root, feedback)
					return RunReport{Status: "candidate_ready"}, integrationRepairPutMarker(root)
				}
				if second > 2 {
					t.Fatal("candidate mutation reached another generation attempt")
				}
			}
			return f.generate(root, false)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Verification became invalid during project integration") || second != 2 {
		t.Fatalf("passing integration committed a changed candidate: err=%v calls=%d", err, second)
	}
	if commits := wfFeatCommits(t, f.project); len(commits) != 0 {
		t.Fatalf("ignored candidate drift allowed a module commit: %v", commits)
	}
	state := integrationRepairState(t, f)
	if state.Pending == nil || len(state.Modules) != 0 || len(state.Steps) != 2 {
		t.Fatalf("candidate drift discarded pending progress: %+v", state)
	}
	root := filepath.Join(f.project, filepath.FromSlash(state.Pending.Task))
	verification, err := CurrentVerification(root)
	if err != nil || verification == nil || verification.Current {
		t.Fatalf("fixture did not invalidate accepted candidate bytes: %v %+v", err, verification)
	}
	data, err := os.ReadFile(filepath.Join(root, "candidate", "alpha", "helper.go"))
	if err != nil || !strings.Contains(string(data), "candidate changed during passing integration") {
		t.Fatalf("candidate mutation was discarded: %v %q", err, data)
	}
	result, err := readJSON[ProcessResult](f.project, state.Pending.Task+"/integration-tests.json")
	if err != nil || !succeeded(result) {
		t.Fatalf("fixture did not reproduce ignored drift after passing tests: %v %+v", err, result)
	}
}
