package portsmith_test

import (
	"context"
	"encoding/json"
	ps "github.com/minifish-org/portsmith-go/internal/portsmith"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPortsmithJudgeIndependentVerification(t *testing.T) {
	p := prepared(t)
	candidate(t, p, badCode)
	v, e := ps.VerifyPort(context.Background(), p, false)
	must(t, e)
	if v.Status != "behavior_failed" || !v.Independent || v.FullParityProven {
		t.Fatalf("candidate self-test incorrectly accepted: %+v", v)
	}
	if _, e = ps.AcceptTask(p, filepath.Join(t.TempDir(), "export")); e == nil {
		t.Fatal("accepted broken candidate")
	}
	candidate(t, p, goodCode)
	v, e = ps.VerifyPort(context.Background(), p, false)
	must(t, e)
	if v.Status != "behavior_verified" || !v.Independent {
		t.Fatalf("correct candidate rejected %+v", v)
	}
	if v.Verifier != "portsmith-native-go-v1" {
		t.Fatal("native verifier needs distinct receipt identity", v.Verifier)
	}
	current, e := ps.CurrentVerification(p)
	must(t, e)
	if current == nil || !current.Current {
		t.Fatal("fresh verification is stale")
	}
	must(t, ps.WriteCandidate(p, "value.go", []byte(badCode)))
	current, e = ps.CurrentVerification(p)
	must(t, e)
	if current == nil || current.Current {
		t.Fatal("stale receipt trusted")
	}
}

func TestPortsmithJudgeMissingAndForgedTests(t *testing.T) {
	p := prepared(t)
	candidate(t, p, goodCode)
	must(t, ps.WriteCandidate(p, "value_test.go", []byte("package port\nimport \"testing\"\nfunc TestSkipped(t *testing.T){t.Skip(\"not evidence\")}\n")))
	v, e := ps.VerifyPort(context.Background(), p, false)
	must(t, e)
	if v.Status == "behavior_verified" {
		t.Fatal("skipped candidate tests accepted")
	}
	p = prepared(t)
	candidate(t, p, goodCode)
	must(t, ps.WriteCandidate(p, "value_test.go", []byte(miniJudge)))
	if v, e = ps.VerifyPort(context.Background(), p, false); e == nil && v.Status == "behavior_verified" {
		t.Fatal("candidate authored its own independent judge")
	}
}

func TestPortsmithJudgeLiveEventStreamOracle(t *testing.T) {
	p := t.TempDir()
	put(t, p, "references/packages/ai/src/utils/event-stream.ts", string(raw(t, "testdata/oracle/event-stream.txt")))
	b, e := ps.EventStreamOracle(context.Background(), p)
	must(t, e)
	want := raw(t, "testdata/oracle/expected.json")
	var gotV, wantV any
	must(t, json.Unmarshal(b, &gotV))
	must(t, json.Unmarshal(want, &wantV))
	if !reflect.DeepEqual(gotV, wantV) {
		t.Fatalf("live TS oracle differs: %s", b)
	}
	// A changed source must change the result. A bundled constant golden is forbidden.
	text := string(raw(t, "testdata/oracle/event-stream.txt"))
	if !strings.Contains(text, "this.extractResult(event)") {
		t.Fatal("mutation target missing")
	}
	text = strings.ReplaceAll(text, "this.extractResult(event)", "(this.extractResult(event) as any) + 100")
	put(t, p, "references/packages/ai/src/utils/event-stream.ts", text)
	b, e = ps.EventStreamOracle(context.Background(), p)
	must(t, e)
	must(t, json.Unmarshal(b, &gotV))
	if reflect.DeepEqual(gotV, wantV) {
		t.Fatal("oracle did not execute supplied TS")
	}
}
