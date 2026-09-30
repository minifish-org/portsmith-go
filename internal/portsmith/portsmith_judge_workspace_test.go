package portsmith_test

import (
	"context"
	ps "github.com/minifish-org/portsmith-go/internal/portsmith"
	"os"
	"path/filepath"
	"testing"
)

const goodCode = "package port\nfunc Value() int { return 42 }\n"
const badCode = "package port\nfunc Value() int { return 41 }\n"
const selfTest = "package port\nimport \"testing\"\nfunc TestCandidate(t *testing.T){if Value()<0{t.Fatal(\"negative\")}}\n"
const miniJudge = "package port\nimport \"testing\"\nfunc TestPortsmithJudgeValue(t *testing.T){if Value()!=42{t.Fatal(\"wrong value\")}}\n"

func prepared(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	s := filepath.Join(d, "source")
	put(t, s, "LICENSE", "fixture license\n")
	put(t, s, "value.ts", "export function Value(){ return 42; }\n")
	put(t, d, "go.mod", "module example.com/candidate\n\ngo 1.24\n")
	put(t, d, "judge/value_judge_test.go", miniJudge)
	p, e := ps.PrepareTask(context.Background(), ps.PrepareOptions{Source: s, Out: filepath.Join(d, "task"), Files: []string{"value.ts"}, Revision: "fixture", Goal: "Port Value returning 42", GoMod: filepath.Join(d, "go.mod"), Judge: filepath.Join(d, "judge"), RequiredJudgeTests: []string{"TestPortsmithJudgeValue"}})
	must(t, e)
	return p
}
func candidate(t *testing.T, p, code string) {
	t.Helper()
	must(t, ps.WriteCandidate(p, "value.go", []byte(code)))
	must(t, ps.WriteCandidate(p, "value_test.go", []byte(selfTest)))
}

func TestPortsmithJudgeTaskSnapshots(t *testing.T) {
	p := prepared(t)
	b, e := ps.LoadTask(p)
	must(t, e)
	m := decode(t, b)
	if m["version"] != float64(1) || m["revision"] != "fixture" {
		t.Fatal(m)
	}
	candidate(t, p, goodCode)
	a, e := ps.Fingerprint(p)
	must(t, e)
	must(t, ps.EditCandidate(p, "value.go", "return 42", "return 43"))
	z, e := ps.Fingerprint(p)
	must(t, e)
	if a == z {
		t.Fatal("fingerprint ignored mutation")
	}
	if e = ps.EditCandidate(p, "value.go", "missing", "x"); e == nil {
		t.Fatal("missing replacement accepted")
	}
	if e = ps.WriteCandidate(p, "../escape.go", []byte(goodCode)); e == nil {
		t.Fatal("path escape accepted")
	}
	if e = ps.WriteCandidate(p, "go.mod", []byte("bad")); e == nil {
		t.Fatal("dependency mutation accepted")
	}
	put(t, p, "references/value.ts", "modified\n")
	if _, e = ps.LoadTask(p); e == nil {
		t.Fatal("reference tampering undetected")
	}
}

func TestPortsmithJudgeFrozenSeedsAndJudges(t *testing.T) {
	p := prepared(t)
	candidate(t, p, goodCode)
	put(t, p, "judge/value_judge_test.go", miniJudge+"// changed\n")
	if _, e := ps.LoadTask(p); e == nil {
		t.Fatal("judge tampering accepted")
	}
	p = prepared(t)
	put(t, p, "candidate/go.mod", "module changed\n\ngo 1.24\n")
	if _, e := ps.LoadTask(p); e == nil {
		t.Fatal("manifest tampering accepted")
	}
	p = prepared(t)
	candidate(t, p, goodCode)
	outside := t.TempDir()
	put(t, outside, "injected.go", goodCode)
	if e := os.Symlink(filepath.Join(outside, "injected.go"), filepath.Join(p, "candidate/injected.go")); e == nil {
		if _, e = ps.CandidateFiles(p); e == nil {
			t.Fatal("candidate symlink accepted")
		}
	}
}
