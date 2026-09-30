package portsmith_test

import (
	"context"
	"encoding/json"
	ps "github.com/minifish-org/portsmith-go/internal/portsmith"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func copyFixture(t *testing.T, from, to string) {
	t.Helper()
	must(t, filepath.WalkDir(from, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		r, e := filepath.Rel(from, p)
		if e != nil {
			return e
		}
		put(t, to, r, string(raw(t, p)))
		return nil
	}))
}
func TestPortsmithJudgeTypeScriptAnalysis(t *testing.T) {
	d := t.TempDir()
	copyFixture(t, "testdata/analysis/input", d)
	put(t, d, "node_modules/ignored/index.ts", "export const ignored = true;\n")
	put(t, d, "dist/ignored.ts", "export const ignored = true;\n")
	b, e := ps.Analyze(context.Background(), d)
	must(t, e)
	got := decode(t, b)
	delete(got, "source")
	want := decode(t, raw(t, "testdata/analysis/expected.json"))
	delete(want, "source")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("analysis diverges from pinned TS compiler oracle\ngot: %s\nwant: %s", encode(t, got), encode(t, want))
	}
	// Hash/config invalidation must reflect a real input edit, not a cached golden.
	put(t, d, "src/value.ts", "export const value = 9;\n")
	changed, e := ps.Analyze(context.Background(), d)
	must(t, e)
	if reflect.DeepEqual(decode(t, changed), decode(t, b)) {
		t.Fatal("analysis ignored edited input")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = ps.Analyze(ctx, d); e == nil {
		t.Fatal("analysis ignored cancellation")
	}
}

func TestPortsmithJudgeDraftPlanning(t *testing.T) {
	d := t.TempDir()
	a := decode(t, raw(t, "testdata/analysis/expected.json"))
	a["source"] = d
	p := filepath.Join(d, "analysis.json")
	put(t, d, "analysis.json", encode(t, a))
	b, e := ps.CreatePlan(p, filepath.Join(d, "plan"), "reviewed-revision")
	must(t, e)
	got := decode(t, b)
	want := decode(t, raw(t, "testdata/analysis/plan.json"))
	delete(got, "source")
	delete(want, "source")
	delete(got, "analysisSha256")
	delete(want, "analysisSha256")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("draft plan differs\ngot %s\nwant %s", encode(t, got), encode(t, want))
	}
	saved := decode(t, raw(t, filepath.Join(d, "plan/plan.json")))
	if saved["analysisSha256"] != digest(raw(t, p)) {
		t.Fatal("plan not bound to exact analysis bytes")
	}
	if _, e = os.Stat(filepath.Join(d, "plan/RULEBOOK.md")); e != nil {
		t.Fatal(e)
	}
	var units []json.RawMessage
	ub, _ := json.Marshal(got["units"])
	must(t, json.Unmarshal(ub, &units))
	if len(units) < 2 {
		t.Fatal("lost directory units")
	}
}
