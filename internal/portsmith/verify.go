// verify.go ports src/verify.ts: isolated production verification. A candidate
// is copied into a scratch module, formatted, compiled, vetted, exercised by
// its own tests and then checked by a frozen independent judge before a
// fingerprint-bound receipt is written. The verifier has its own identity
// (portsmith-native-go-v1) so a TypeScript executor receipt can never validate
// Go output.
package portsmith

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// VERIFIER_VERSION identifies this Go verifier. Old TypeScript receipts used a
// different identity and must be re-verified.
const VERIFIER_VERSION = "portsmith-native-go-v1"

// judgeTestNamePattern matches candidate test functions that impersonate the
// independent judge.
var judgeTestNamePattern = regexp.MustCompile(`(?m)^\s*func\s+TestPortsmithJudge\w*\s*\(`)

func isSeedName(seeds []NamedDigest, name string) bool {
	for _, seed := range seeds {
		if seed.Name == name {
			return true
		}
	}
	return false
}

func judgeFileNamed(task PortTask, name string) bool {
	for _, judge := range task.JudgeFiles {
		if judge.Name == name {
			return true
		}
	}
	return false
}

// requiredTestsPassed mirrors `requiredJudgeTests.every(name => passedNames
// .includes(name))`. An absent list is trivially satisfied.
func requiredTestsPassed(task PortTask, passedNames []string) bool {
	for _, name := range stringList(task.RequiredJudgeTests) {
		if !containsString(passedNames, name) {
			return false
		}
	}
	return true
}

// intPtr returns a pointer to a fresh int, used for ProcessResult exit codes.
func intPtr(value int) *int { return &value }

// VerifyPort runs the full isolated verification pipeline against a prepared
// task. Writable candidate Go files are formatted in place first; everything
// else happens in a scratch copy. The returned report is written to
// verification.json and carries the pre-verification fingerprint.
func VerifyPort(ctx context.Context, rootInput string, download bool) (Verification, error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	root, _, task, err := loadTask(rootInput)
	if err != nil {
		return Verification{}, err
	}

	// Normalize only writable candidate Go files; frozen seeds and judges are
	// immutable.
	files, err := CandidateFiles(root)
	if err != nil {
		return Verification{}, err
	}
	seeds := namedDigestList(task.SeedFiles)
	var writableGo []string
	for _, file := range files {
		if strings.HasSuffix(file.Name, ".go") && !isSeedName(seeds, file.Name) {
			writableGo = append(writableGo, "./"+file.Name)
		}
	}
	if len(writableGo) > 0 {
		formatted := Execute(ctx, ProcessOptions{
			Command:  "gofmt",
			Args:     append([]string{"-w"}, writableGo...),
			Cwd:      filepath.Join(root, "candidate"),
			Download: download,
		})
		if !succeeded(formatted) {
			return Verification{}, fmt.Errorf("Go formatting/syntax check failed: %s", formatted.Log)
		}
	}

	before, err := Fingerprint(root)
	if err != nil {
		return Verification{}, err
	}
	files, err = CandidateFiles(root)
	if err != nil {
		return Verification{}, err
	}
	hasImplementation := false
	for _, file := range files {
		if strings.HasSuffix(file.Name, ".go") && !strings.HasSuffix(file.Name, "_test.go") {
			hasImplementation = true
			break
		}
	}
	if !hasImplementation {
		return Verification{}, errors.New("Candidate directory contains no Go implementation")
	}

	report := Verification{
		Version:          1,
		Verifier:         VERIFIER_VERSION,
		At:               time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Fingerprint:      before,
		Status:           "compile_failed",
		FullParityProven: false,
		Phases:           []Phase{},
	}
	fail := func(cause error) (Verification, error) {
		if current, fingerprintErr := Fingerprint(root); fingerprintErr == nil && current == before {
			report.Phases = append(report.Phases, Phase{
				Name:   "verifier-error",
				Result: ProcessResult{Code: intPtr(1), Log: cause.Error()},
			})
			_ = AtomicJSON(filepath.Join(root, "verification.json"), report)
		}
		return report, cause
	}

	temp, err := os.MkdirTemp("", "portsmith-verify-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(temp)

	conflict := errors.New("Candidate conflicts with an independent verifier path or reserved test prefix")
	for _, file := range files {
		if strings.HasSuffix(file.Name, "_test.go") && judgeTestNamePattern.Match(file.Data) {
			return fail(conflict)
		}
		if candidateIsReserved(file.Name) || judgeFileNamed(task, file.Name) {
			return fail(conflict)
		}
	}
	if err := copyFiles(temp, files); err != nil {
		return fail(err)
	}

	build := executeGo(ctx, temp, []string{"-run", "^$"}, download, false)
	report.Phases = append(report.Phases, Phase{Name: "compile", Result: build})
	var vet ProcessResult
	vetRan := false
	if succeeded(build) {
		vet = Execute(ctx, ProcessOptions{
			Command:  "go",
			Args:     []string{"vet", "-mod=readonly", "./..."},
			Cwd:      temp,
			Download: download,
		})
		report.Phases = append(report.Phases, Phase{Name: "vet", Result: vet})
		vetRan = true
	}

	if succeeded(build) && vetRan && succeeded(vet) {
		report.Status = "tests_failed"
		tests := executeGo(ctx, temp, nil, download, false)
		report.Phases = append(report.Phases, Phase{Name: "candidate-tests", Result: tests})
		counts := testResults(tests, "")
		if succeeded(tests) && counts.Passed > 0 && counts.Skipped == 0 {
			report.Status = "tests_passed"
			if task.Example != "" || len(task.JudgeFiles) > 0 {
				report.Status = "behavior_failed"
				if task.Example != "" {
					if _, err := JudgeCheck(ctx, root); err != nil {
						return fail(err)
					}
					cases, err := eventStreamOracle(ctx, root)
					if err != nil {
						return fail(err)
					}
					report.OracleCases = len(cases)
					if err := injectOracle(temp, cases); err != nil {
						return fail(err)
					}
					if err := AtomicJSON(filepath.Join(root, "oracle.json"), cases); err != nil {
						return fail(err)
					}
				} else {
					for _, judge := range task.JudgeFiles {
						file, err := CheckedFile(root, "judge/"+judge.Name)
						if err != nil {
							return fail(err)
						}
						data, err := os.ReadFile(file)
						if err != nil {
							return fail(err)
						}
						if err := copyFiles(temp, []File{{Name: judge.Name, Data: data, SHA256: Hash(data)}}); err != nil {
							return fail(err)
						}
					}
				}
				judge := executeGo(ctx, temp, []string{"-run", "^TestPortsmithJudge"}, download, false)
				report.Phases = append(report.Phases, Phase{Name: "independent-behavior", Result: judge})
				judgeCounts := testResults(judge, "TestPortsmithJudge")
				report.Independent = true
				minPassed := 1
				if task.Example != "" {
					minPassed = 9
				}
				if succeeded(judge) && judgeCounts.Passed >= minPassed && judgeCounts.Skipped == 0 &&
					requiredTestsPassed(task, judgeCounts.PassedNames) {
					report.Status = "behavior_verified"
					if task.Race {
						race := executeGo(ctx, temp, []string{"-run", "^TestPortsmithJudge"}, download, true)
						report.Phases = append(report.Phases, Phase{Name: "race", Result: race})
						raceCounts := testResults(race, "TestPortsmithJudge")
						if !succeeded(race) || raceCounts.Skipped > 0 || !requiredTestsPassed(task, raceCounts.PassedNames) {
							report.Status = "behavior_failed"
						}
					}
				}
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	after, err := Fingerprint(root)
	if err != nil {
		return fail(err)
	}
	if after != before {
		return fail(errors.New("Code changed during verification; report invalidated, retry verification"))
	}
	if err := AtomicJSON(filepath.Join(root, "verification.json"), report); err != nil {
		return report, err
	}
	return report, nil
}

// CurrentVerificationResult pairs a stored receipt with whether it still
// matches the current candidate fingerprint and verifier identity.
type CurrentVerificationResult struct {
	Current bool
	Report  Verification
}

// CurrentVerification loads verification.json and reports whether it is still
// current. A missing receipt returns (nil, nil), matching the upstream
// `undefined` result.
func CurrentVerification(rootInput string) (*CurrentVerificationResult, error) {
	report, err := readJSON[Verification](rootInput, "verification.json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	fingerprint, err := Fingerprint(rootInput)
	if err != nil {
		return nil, err
	}
	return &CurrentVerificationResult{
		Report:  report,
		Current: report.Verifier == VERIFIER_VERSION && report.Fingerprint == fingerprint,
	}, nil
}
