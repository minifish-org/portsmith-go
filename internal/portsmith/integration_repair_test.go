package portsmith

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const integrationRepairMarker = "// integration-ready"

// This frozen historical check deliberately lives outside the new candidate's
// judge set. Both candidate steps can satisfy their cumulative contracts while
// the real project still rejects a missing delivery detail.
func integrationRepairFixture(t *testing.T) *wfModuleFixture {
	t.Helper()
	f := wfNewModuleFixture(t)
	f.ready()
	f.planDoc.Modules = f.planDoc.Modules[:1]
	f.planDoc.Batches = f.planDoc.Batches[:1]
	delete(f.workflow.Batches, "consumer")
	f.save()
	testPut(t, f.project, "legacy_test.go", `package legacy
import (
 "os"
 "strings"
 "testing"
)
func TestPortsmithJudgeLegacyDelivery(t *testing.T) {
 data, err := os.ReadFile("alpha/helper.go")
 if err != nil { t.Fatal(err) }
 if !strings.Contains(string(data), "// integration-ready") {
  t.Fatal("historical integration check requires delivery marker")
 }
}
`)
	wfGit(t, f.project, "add", "legacy_test.go")
	wfGit(t, f.project, "commit", "-qm", "historical integration gate")
	return f
}

func integrationRepairState(t *testing.T, f *wfModuleFixture) moduleState {
	t.Helper()
	state, err := readJSON[moduleState](f.project, ".portsmith/modules.json")
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func integrationRepairPutMarker(root string) error {
	return WriteCandidate(root, "alpha/helper.go", []byte("package alpha\n"+integrationRepairMarker+"\nfunc helper() int {return 42}\n"))
}

func integrationRepairAssertClean(t *testing.T, f *wfModuleFixture) {
	t.Helper()
	if status := wfGit(t, f.project, "status", "--porcelain"); status != "" {
		t.Fatalf("failed integration leaked into the target: %s", status)
	}
	if commits := wfFeatCommits(t, f.project); len(commits) != 0 {
		t.Fatalf("unaccepted module was committed: %v", commits)
	}
	for _, name := range f.planDoc.Batches[0].Outputs {
		if _, err := os.Stat(filepath.Join(f.project, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("unaccepted output survived rollback: %s: %v", name, err)
		}
	}
}

func integrationRepairAssertFeedback(t *testing.T, f *wfModuleFixture, root, feedback string) (string, []byte) {
	t.Helper()
	state := integrationRepairState(t, f)
	if state.Pending != nil || state.Repair == nil || len(state.Steps) != 1 || len(state.Modules) != 0 {
		t.Fatalf("unexpected repair checkpoint: %+v", state)
	}
	if !strings.Contains(feedback, "TestPortsmithJudgeLegacyDelivery") || !strings.Contains(feedback, "historical integration check requires delivery marker") {
		t.Fatalf("real integration failure was not fed back to the agent: %s", feedback)
	}
	integrationRepairAssertClean(t, f)
	verification, err := CurrentVerification(root)
	if err != nil || verification == nil || verification.Report.Status != "behavior_verified" {
		t.Fatalf("fixture did not reproduce a candidate-only pass: %+v %v", verification, err)
	}
	_, _, task, err := loadTask(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := stringList(task.RequiredJudgeTests); !containsString(got, "TestPortsmithJudgeFirst") || !containsString(got, "TestPortsmithJudgeSecond") {
		t.Fatalf("repair lost cumulative independent gates: %v", got)
	}
	if err := WriteCandidate(root, "legacy_test.go", []byte("package legacy\n")); err == nil {
		t.Fatal("repair could overwrite the historical integration gate")
	}
	report := filepath.Join(f.project, filepath.FromSlash(state.Repair.Report))
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var result ProcessResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Code == nil || *result.Code == 0 || !strings.Contains(result.Log, "historical integration check requires delivery marker") {
		t.Fatalf("failure evidence does not contain the actual rejected gate: %+v", result)
	}
	return report, data
}

func TestWorkflowV2IntegrationFailureRepairsCandidate(t *testing.T) {
	f := integrationRepairFixture(t)
	legacy, err := os.ReadFile(filepath.Join(f.project, "legacy_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	var prefix []byte
	var failurePath string
	var failure []byte
	result, err := Migrate(context.Background(), MigrationOptions{
		Plan: f.plan, Commit: true,
		Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
			_, _, task, err := loadTask(root)
			if err != nil {
				return RunReport{}, err
			}
			calls[task.Unit]++
			if strings.HasSuffix(task.Unit, "/Second") {
				state := integrationRepairState(t, f)
				if len(state.Steps) != 1 {
					t.Fatalf("first step checkpoint missing: %+v", state)
				}
				got, err := json.Marshal(state.Steps[0])
				if err != nil {
					t.Fatal(err)
				}
				if prefix == nil {
					prefix = got
				} else if string(got) != string(prefix) {
					t.Fatalf("repair changed the accepted prefix: %s != %s", got, prefix)
				}
				if calls[task.Unit] == 2 {
					failurePath, failure = integrationRepairAssertFeedback(t, f, root, feedback)
					return RunReport{Status: "candidate_ready"}, integrationRepairPutMarker(root)
				}
				if calls[task.Unit] > 2 {
					t.Fatal("repair did not converge after fixing the real integration failure")
				}
			}
			return f.generate(root, false)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wfDecode(t, result)["status"] != "complete" || calls["alpha/foundation/First"] != 1 || calls["alpha/foundation/Second"] != 2 {
		t.Fatalf("integration repair did not finish in the same invocation: %s calls=%v", result, calls)
	}
	state := integrationRepairState(t, f)
	if state.Pending != nil || state.Repair != nil || len(state.Modules) != 1 || len(state.Steps) != 2 {
		t.Fatalf("final repair state was not closed: %+v", state)
	}
	gotPrefix, _ := json.Marshal(state.Steps[0])
	if string(gotPrefix) != string(prefix) {
		t.Fatal("successful delivery rewrote the accepted prefix")
	}
	if commits := wfFeatCommits(t, f.project); len(commits) != 1 || commits[0] != "feat: port alpha" {
		t.Fatalf("repair created intermediate commits: %v", commits)
	}
	if status := wfGit(t, f.project, "status", "--porcelain"); status != "" {
		t.Fatalf("successful repair left target dirty: %s", status)
	}
	if got, err := os.ReadFile(filepath.Join(f.project, "legacy_test.go")); err != nil || string(got) != string(legacy) {
		t.Fatalf("historical test changed: %v", err)
	}
	if got, err := os.ReadFile(failurePath); err != nil || string(got) != string(failure) {
		t.Fatalf("later passing checks erased integration failure evidence: %v", err)
	}
	if _, err := Migrate(context.Background(), MigrationOptions{Plan: f.plan, Commit: true, Generate: wfNoModel}); err != nil {
		t.Fatalf("completed migration repeated a repair: %v", err)
	}
}

func TestWorkflowV2IntegrationRepairAttemptBudgetAndResume(t *testing.T) {
	for _, budget := range []int{1, 2} {
		t.Run(itoa(budget), func(t *testing.T) {
			f := integrationRepairFixture(t)
			second := 0
			_, err := Migrate(context.Background(), MigrationOptions{
				Plan: f.plan, Commit: true, MaxAttempts: budget,
				Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
					if strings.HasSuffix(root, "/Second") {
						second++
						if second > 1 {
							integrationRepairAssertFeedback(t, f, root, feedback)
						}
					}
					return f.generate(root, false)
				},
			})
			if err == nil || !strings.Contains(err.Error(), "generation/repair attempts") || !strings.Contains(err.Error(), "integration") {
				t.Fatalf("explicit repair budget was not honored: %v", err)
			}
			if second != budget {
				t.Fatalf("integration cycles reset the attempt budget: calls=%d budget=%d", second, budget)
			}
			integrationRepairAssertClean(t, f)
			state := integrationRepairState(t, f)
			if state.Pending != nil || state.Repair == nil || len(state.Steps) != 1 || state.Attempts["alpha/foundation/Second"] != budget {
				t.Fatalf("repair progress not retained at the explicit limit: %+v", state)
			}
			resumeCalls := 0
			result, err := Migrate(context.Background(), MigrationOptions{
				Plan: f.plan, Commit: true, MaxAttempts: 1,
				Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
					resumeCalls++
					if !strings.HasSuffix(root, "/Second") {
						t.Fatalf("resume regenerated accepted prefix: %s", root)
					}
					integrationRepairAssertFeedback(t, f, root, feedback)
					return RunReport{Status: "candidate_ready"}, integrationRepairPutMarker(root)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if wfDecode(t, result)["status"] != "complete" || resumeCalls != 1 {
				t.Fatalf("resume reused candidate-only acceptance without repairing integration: %s calls=%d", result, resumeCalls)
			}
			state = integrationRepairState(t, f)
			if state.Repair != nil || state.Attempts["alpha/foundation/Second"] != budget+1 {
				t.Fatalf("repair counters were not durable: %+v", state)
			}
		})
	}
}

func TestWorkflowV2IntegrationRepairModelErrorKeepsCleanTarget(t *testing.T) {
	f := integrationRepairFixture(t)
	second := 0
	_, err := Migrate(context.Background(), MigrationOptions{
		Plan: f.plan, Commit: true,
		Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
			if strings.HasSuffix(root, "/Second") {
				second++
				if second == 2 {
					integrationRepairAssertFeedback(t, f, root, feedback)
					return RunReport{}, errors.New("offline simulated provider failure")
				}
			}
			return f.generate(root, false)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "offline simulated provider failure") {
		t.Fatalf("model error hidden by integration retry: %v", err)
	}
	integrationRepairAssertClean(t, f)
	state := integrationRepairState(t, f)
	if state.Pending != nil || state.Repair == nil || len(state.Steps) != 1 {
		t.Fatalf("model error discarded resumable repair: %+v", state)
	}
	result, err := Migrate(context.Background(), MigrationOptions{
		Plan: f.plan, Commit: true, MaxAttempts: 1,
		Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
			integrationRepairAssertFeedback(t, f, root, feedback)
			return RunReport{Status: "candidate_ready"}, integrationRepairPutMarker(root)
		},
	})
	if err != nil || wfDecode(t, result)["status"] != "complete" {
		t.Fatalf("provider failure repair could not resume: %v %s", err, result)
	}
}

func TestWorkflowV2IntegrationRepairRejectsChangedEvidence(t *testing.T) {
	f := integrationRepairFixture(t)
	_, err := Migrate(context.Background(), MigrationOptions{
		Plan: f.plan, Commit: true, MaxAttempts: 1,
		Generate: func(ctx context.Context, root, feedback string) (RunReport, error) { return f.generate(root, false) },
	})
	if err == nil {
		t.Fatal("fixture unexpectedly passed historical integration")
	}
	state := integrationRepairState(t, f)
	if state.Repair == nil {
		t.Fatal("missing saved integration repair")
	}
	testPut(t, f.project, state.Repair.Report, "{\"code\":0,\"log\":\"forged acceptance\"}\n")
	calls := 0
	_, err = Migrate(context.Background(), MigrationOptions{
		Plan: f.plan, Commit: true,
		Generate: func(context.Context, string, string) (RunReport, error) {
			calls++
			return RunReport{}, errors.New("must not generate after evidence drift")
		},
	})
	if err == nil || calls != 0 {
		t.Fatalf("changed integration evidence reached the model: err=%v calls=%d", err, calls)
	}
	integrationRepairAssertClean(t, f)
}

func TestWorkflowV2IntegrationCancellationPreservesPendingCommit(t *testing.T) {
	f := integrationRepairFixture(t)
	testPut(t, f.project, "legacy_test.go", `package legacy
import (
 "os"
 "testing"
 "time"
)
func TestPortsmithJudgeLegacyDelivery(t *testing.T) {
 if err := os.WriteFile(".portsmith/integration-started", []byte("started"), 0600); err != nil { t.Fatal(err) }
 for {
  _, err := os.Stat(".portsmith/block")
  if os.IsNotExist(err) { return }
  if err != nil { t.Fatal(err) }
  time.Sleep(5*time.Millisecond)
 }
}
`)
	wfGit(t, f.project, "add", "legacy_test.go")
	wfGit(t, f.project, "commit", "-qm", "blocking integration fixture")
	testPut(t, f.project, ".portsmith/block", "wait for cancellation\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		guard := time.NewTimer(30 * time.Second)
		defer guard.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := os.Stat(filepath.Join(f.project, ".portsmith/integration-started")); err == nil {
					started <- nil
					cancel()
					return
				} else if !os.IsNotExist(err) {
					started <- err
					cancel()
					return
				}
			case <-guard.C:
				started <- errors.New("integration test did not start within the test hang guard")
				cancel()
				return
			case <-ctx.Done():
				started <- ctx.Err()
				return
			}
		}
	}()
	calls := 0
	_, err := Migrate(ctx, MigrationOptions{
		Plan: f.plan, Commit: true,
		Generate: func(ctx context.Context, root, feedback string) (RunReport, error) {
			calls++
			if calls > 2 {
				return RunReport{}, errors.New("cancellation must not become a model repair")
			}
			return f.generate(root, false)
		},
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cancel") {
		cancel()
		t.Fatalf("integration cancellation was not returned: %v", err)
	}
	if observed := <-started; observed != nil {
		t.Fatal(observed)
	}
	if calls != 2 {
		t.Fatalf("cancelled integration triggered generation: %d calls", calls)
	}
	state := integrationRepairState(t, f)
	if state.Pending == nil || state.Pending.Repair != nil || state.Repair != nil || len(state.Steps) != 2 || len(state.Modules) != 0 {
		t.Fatalf("cancellation reopened or discarded accepted work: %+v", state)
	}
	if commits := wfFeatCommits(t, f.project); len(commits) != 0 {
		t.Fatalf("cancelled integration committed a module: %v", commits)
	}
	root := filepath.Join(f.project, filepath.FromSlash(state.Pending.Task))
	verification, err := CurrentVerification(root)
	if err != nil || verification == nil || !verification.Current || verification.Report.Status != "behavior_verified" {
		t.Fatalf("cancellation lost the accepted candidate: %+v %v", verification, err)
	}
	integration, err := readJSON[ProcessResult](root, "integration-tests.json")
	if err != nil || !integration.Cancelled {
		t.Fatalf("cancelled integration was misreported as a test failure: %+v %v", integration, err)
	}
	if err := os.Remove(filepath.Join(f.project, ".portsmith/block")); err != nil {
		t.Fatal(err)
	}
	result, err := Migrate(context.Background(), MigrationOptions{Plan: f.plan, Commit: true, Generate: wfNoModel})
	if err != nil || wfDecode(t, result)["status"] != "complete" {
		t.Fatalf("cancelled integration could not resume without generation: %v %s", err, result)
	}
	state = integrationRepairState(t, f)
	if state.Pending != nil || state.Repair != nil || len(state.Steps) != 2 || len(state.Modules) != 1 {
		t.Fatalf("resumed integration did not close the transaction: %+v", state)
	}
	if commits := wfFeatCommits(t, f.project); len(commits) != 1 {
		t.Fatalf("resume created duplicate module commits: %v", commits)
	}
	if status := wfGit(t, f.project, "status", "--porcelain"); status != "" {
		t.Fatalf("resumed integration left target dirty: %s", status)
	}
}
