// event_stream.go ports src/event-stream.ts: the fixed seven-scenario
// EventStream oracle, the independent Go judge template and the known-broken
// negative control. The oracle executes the supplied original TypeScript file
// inside an isolated Goja runtime after transpiling it with the embedded
// TypeScript compiler. No Node, tsx or subprocess JavaScript runtime is used.
package portsmith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// oracleStep is one fixed oracle operation (`push`, `end`, `next`, `wait`,
// `take`, `result`). The upstream `value` member is optional.
type oracleStep struct {
	Op    string `json:"op"`
	Value *int   `json:"value,omitempty"`
}

// oracleCase is one fixed EventStream scenario together with the trace the live
// source produced (`complete`, `steps`, `expected`).
type oracleCase struct {
	Name     string       `json:"name"`
	Complete int          `json:"complete"`
	Steps    []oracleStep `json:"steps"`
	Expected []any        `json:"expected,omitempty"`
}

func pushStep(value int) oracleStep { return oracleStep{Op: "push", Value: &value} }

func endStep(value int) oracleStep { return oracleStep{Op: "end", Value: &value} }

var waitStep = oracleStep{Op: "wait"}
var takeStep = oracleStep{Op: "take"}
var nextStep = oracleStep{Op: "next"}
var resultStep = oracleStep{Op: "result"}
var endWithoutValue = oracleStep{Op: "end"}

// eventStreamCases is the direct port of the upstream `cases` constant. The
// order of scenarios and steps is frozen; it is the contract the Go judge
// validates against.
func eventStreamCases() []oracleCase {
	return []oracleCase{
		{
			Name:     "completion retains event and ignores later pushes",
			Complete: 3,
			Steps: []oracleStep{
				pushStep(1), pushStep(2), pushStep(3), pushStep(4),
				resultStep, nextStep, nextStep, nextStep, nextStep,
			},
		},
		{
			Name:     "interleaved draining",
			Complete: -1,
			Steps: []oracleStep{
				pushStep(1), pushStep(2), nextStep, pushStep(3), nextStep,
				nextStep, endStep(7), nextStep, resultStep,
			},
		},
		{
			Name:     "explicit end preserves queue",
			Complete: -1,
			Steps: []oracleStep{
				pushStep(5), pushStep(6), endStep(42), pushStep(7),
				resultStep, nextStep, nextStep, nextStep,
			},
		},
		{
			Name:     "registered waiters are FIFO",
			Complete: -1,
			Steps: []oracleStep{
				waitStep, waitStep, pushStep(7), pushStep(8),
				takeStep, takeStep, endWithoutValue, nextStep,
			},
		},
		{
			Name:     "end without result wakes waiters",
			Complete: -1,
			Steps: []oracleStep{
				waitStep, waitStep, endWithoutValue, takeStep, takeStep,
			},
		},
		{
			Name:     "first result wins",
			Complete: 3,
			Steps: []oracleStep{
				pushStep(3), endStep(99), resultStep, nextStep, nextStep,
			},
		},
		{
			Name:     "end can later resolve an absent result",
			Complete: -1,
			Steps: []oracleStep{
				endWithoutValue, endStep(0), resultStep, nextStep,
			},
		},
	}
}

// asyncIteratorShim installs a well-known-symbol stand-in before any transpiled
// module runs. Goja does not yet expose Symbol.asyncIterator, and the
// downleveled async generator checks for it.
const asyncIteratorShim = `if (typeof Symbol === "function" && !Symbol.asyncIterator) {
  Symbol.asyncIterator = Symbol("Symbol.asyncIterator");
}`

// eventStreamDriver mirrors the loop body of the upstream `eventStreamOracle`
// function. It runs entirely inside the isolated oracle runtime and stores the
// serialized trace on the global object. `finally` marks completion so Go can
// distinguish a settled run from an unresolved asynchronous result.
const eventStreamDriver = `
globalThis.__portsmithRun = async function (cases) {
  try {
    var EventStream = globalThis.__portsmithEventStream;
    var results = [];
    for (var c = 0; c < cases.length; c++) {
      var scenario = cases[c];
      var stream = new EventStream(function (n) { return n === scenario.complete; }, function (n) { return n; });
      var iterator = stream[Symbol.asyncIterator]();
      var waiting = [];
      var trace = [];
      var item = function (value) { return { done: value.done === true, value: value.done ? 0 : value.value }; };
      for (var s = 0; s < scenario.steps.length; s++) {
        var step = scenario.steps[s];
        if (step.op === "push") stream.push(step.value);
        if (step.op === "end") stream.end(step.value);
        if (step.op === "next") trace.push(item(await iterator.next()));
        if (step.op === "wait") waiting.push(stream[Symbol.asyncIterator]().next());
        if (step.op === "take") trace.push(item(await waiting.shift()));
        if (step.op === "result") trace.push({ result: await stream.result() });
      }
      results.push({ name: scenario.name, complete: scenario.complete, steps: scenario.steps, expected: trace });
    }
    globalThis.__portsmithOracleResult = JSON.stringify(results);
  } catch (error) {
    globalThis.__portsmithOracleError = String(error && error.message ? error.message : error);
  } finally {
    globalThis.__portsmithOracleDone = true;
  }
};
`

// transpileEventStream transpiles a TypeScript source string to CommonJS with
// the fixed embedded compiler. The compiler runs in its own runtime; the
// returned JavaScript is executed elsewhere so source code can never observe
// the compiler host or any Go-provided filesystem callback.
func transpileEventStream(ctx context.Context, source string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	program, err := compiledTypeScript()
	if err != nil {
		return "", fmt.Errorf("compile typescript: %w", err)
	}
	vm := goja.New()
	if _, err := vm.RunString(asyncIteratorShim); err != nil {
		return "", err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-done:
		}
	}()
	defer close(done)
	if _, err := vm.RunProgram(program); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("load typescript: %w", err)
	}
	vm.Set("__portsmithSource", source)
	value, err := vm.RunString(`ts.transpileModule(__portsmithSource, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2015 } }).outputText`)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("transpile event stream: %w", err)
	}
	return value.String(), nil
}

// eventStreamOracle runs the supplied original TypeScript file and returns the
// seven scenario traces. The source is read from the task reference snapshot
// and executed in a fresh runtime that has no filesystem, process or module
// loader bindings.
func eventStreamOracle(ctx context.Context, root string) ([]oracleCase, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sourceFile, err := CheckedFile(root, "references/packages/ai/src/utils/event-stream.ts")
	if err != nil {
		return nil, err
	}
	source, err := os.ReadFile(sourceFile)
	if err != nil {
		return nil, err
	}
	transpiled, err := transpileEventStream(ctx, string(source))
	if err != nil {
		return nil, err
	}
	caseJSON, err := json.Marshal(eventStreamCases())
	if err != nil {
		return nil, err
	}

	vm := goja.New()
	if _, err := vm.RunString(asyncIteratorShim); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-done:
		}
	}()
	defer close(done)

	module := "var module = { exports: {} }; var exports = module.exports;\n" +
		transpiled + "\nglobalThis.__portsmithEventStream = exports.EventStream;\n" + eventStreamDriver
	if _, err := vm.RunString(module); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("load event stream oracle: %w", err)
	}
	vm.Set("__portsmithCaseJSON", string(caseJSON))
	if _, err := vm.RunString("__portsmithRun(JSON.parse(__portsmithCaseJSON));"); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("run event stream oracle: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finished := vm.Get("__portsmithOracleDone")
	if finished == nil || goja.IsUndefined(finished) || !finished.ToBoolean() {
		return nil, errors.New("source oracle did not settle; unexpected unresolved asynchronous result")
	}
	if failure := vm.Get("__portsmithOracleError"); failure != nil && !goja.IsUndefined(failure) && !goja.IsNull(failure) {
		return nil, fmt.Errorf("source oracle failed: %s", failure.String())
	}
	result := vm.Get("__portsmithOracleResult")
	if result == nil || goja.IsUndefined(result) {
		return nil, errors.New("source oracle produced no result")
	}
	var cases []oracleCase
	if err := json.Unmarshal([]byte(result.String()), &cases); err != nil {
		return nil, fmt.Errorf("decode source oracle result: %w", err)
	}
	return cases, nil
}

// EventStreamOracle exposes the live source oracle as JSON so the independent
// judge (and the verification command) can compare Go output with the original
// TypeScript behavior.
func EventStreamOracle(ctx context.Context, rootInput string) (json.RawMessage, error) {
	root, err := filepath.EvalSymlinks(rootInput)
	if err != nil {
		return nil, err
	}
	cases, err := eventStreamOracle(ctx, root)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cases)
}

// goOracle is the Go judge template injected for the built-in EventStream
// example. It is the exact upstream fixture with the reserved test names
// substituted. Candidate code is compiled against it inside the isolated
// verification copy.
const goOracle = `package port
import ("context"; "encoding/json"; "os"; "reflect"; "testing"; "time")
func TestPortOracle(t *testing.T) {
 var cases []struct { Name string; Complete int; Steps []struct { Op string; Value *int }; Expected []any }
 data, err := os.ReadFile("port_oracle.json"); if err != nil { t.Fatal(err) }
 if err := json.Unmarshal(data, &cases); err != nil { t.Fatal(err) }
 for _, c := range cases { t.Run(c.Name, func(t *testing.T) {
  s := NewEventStream[int,int](func(n int) bool { return n == c.Complete }, func(n int) int { return n })
  waiting := []<-chan StreamItem[int]{}; trace := []any{}
  receive := func(ch <-chan StreamItem[int]) {
   select { case v, ok := <-ch:
    if !ok { t.Fatal("Next closed without an item") }; value := v.Value; if v.Done { value = 0 }
    trace = append(trace, map[string]any{"done":v.Done,"value":value})
   case <-time.After(time.Second): t.Fatal("Next blocked") }
  }
  for _, step := range c.Steps { switch step.Op {
   case "push": s.Push(*step.Value)
   case "end": s.End(step.Value)
   case "wait": waiting = append(waiting, s.Next())
   case "take": receive(waiting[0]); waiting = waiting[1:]
   case "next": receive(s.Next())
   case "result":
    ctx, cancel := context.WithTimeout(context.Background(),time.Second)
    value, err := s.Result(ctx); cancel(); if err != nil { t.Fatal(err) }
    trace = append(trace,map[string]any{"result":value})
  } }
  encoded, _ := json.Marshal(trace); var normalized []any; _ = json.Unmarshal(encoded,&normalized)
  if !reflect.DeepEqual(normalized,c.Expected) { t.Fatalf("trace %s != TS %v",encoded,c.Expected) }
 }) }
}
func TestPortResultCancellation(t *testing.T) {
 s := NewEventStream[int,int](func(int)bool{return false},func(n int)int{return n})
 s.End(nil)
 ctx, cancel := context.WithTimeout(context.Background(),20*time.Millisecond); defer cancel()
 if _, err := s.Result(ctx); err == nil { t.Fatal("End(nil) must not resolve an absent result") }
}
`

// brokenOracleSample is the deliberately faulty EventStream implementation the
// judge-check negative control compiles. It satisfies the interface but always
// reports an empty, finished stream, so the oracle must fail it.
const brokenOracleSample = `package port
import "context"
type StreamItem[T any] struct { Value T; Done bool }
type EventStream[T any,R any] struct{}
func NewEventStream[T any,R any](func(T)bool,func(T)R)*EventStream[T,R]{return &EventStream[T,R]{}}
func(s *EventStream[T,R])Push(T){}
func(s *EventStream[T,R])End(*R){}
func(s *EventStream[T,R])Next()<-chan StreamItem[T]{ch:=make(chan StreamItem[T],1);ch<-StreamItem[T]{Done:true};return ch}
func(s *EventStream[T,R])Result(context.Context)(R,error){var r R;return r,nil}
`

// eventStreamBaseline is the reviewed trace of the original TypeScript source.
// It guards against silently judging a different source revision; it is not
// used as the oracle itself.
const eventStreamBaseline = `[[{"result":3},{"done":false,"value":1},{"done":false,"value":2},{"done":false,"value":3},{"done":true,"value":0}],[{"done":false,"value":1},{"done":false,"value":2},{"done":false,"value":3},{"done":true,"value":0},{"result":7}],[{"result":42},{"done":false,"value":5},{"done":false,"value":6},{"done":true,"value":0}],[{"done":false,"value":7},{"done":false,"value":8},{"done":true,"value":0}],[{"done":true,"value":0},{"done":true,"value":0}],[{"result":3},{"done":false,"value":3},{"done":true,"value":0}],[{"result":0},{"done":true,"value":0}]]`

// judgeCheckReport is the receipt written by JudgeCheck. It records that the
// live source oracle matches the reviewed baseline and that the injected judge
// rejects a known-broken implementation.
type judgeCheckReport struct {
	Version                         int               `json:"version"`
	Verifier                        string            `json:"verifier"`
	At                              string            `json:"at"`
	Source                          []ReferenceRecord `json:"source"`
	Rules                           string            `json:"rules"`
	BaselineCases                   int               `json:"baselineCases"`
	KnownBrokenImplementationCaught bool              `json:"knownBrokenImplementationCaught"`
	FailedTests                     int               `json:"failedTests"`
	OracleSHA256                    string            `json:"oracleSha256"`
}

// injectOracle writes the oracle fixture and the renamed Go judge into the
// isolated verification copy.
func injectOracle(temp string, cases []oracleCase) error {
	data, err := json.Marshal(cases)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temp, "port_oracle.json"), data, 0o644); err != nil {
		return err
	}
	judge := strings.ReplaceAll(goOracle, "TestPortOracle", "TestPortsmithJudgeOracle")
	judge = strings.ReplaceAll(judge, "TestPortResultCancellation", "TestPortsmithJudgeCancellation")
	return os.WriteFile(filepath.Join(temp, "portsmith_judge_test.go"), []byte(judge), 0o644)
}

// JudgeCheck runs the built-in EventStream oracle and validates that the
// injected judge catches the known-broken sample. Unexpected source drift is
// reported instead of being silently accepted.
func JudgeCheck(ctx context.Context, rootInput string) (json.RawMessage, error) {
	root, _, task, err := loadTask(rootInput)
	if err != nil {
		return nil, err
	}
	if task.Example != "event-stream" {
		return nil, errors.New("judge-check supports the built-in EventStream baseline; custom judges need their own baseline and mutation evidence")
	}
	cases, err := eventStreamOracle(ctx, root)
	if err != nil {
		return nil, err
	}
	var actual []any
	for _, c := range cases {
		actual = append(actual, c.Expected)
	}
	actualJSON, err := json.Marshal(actual)
	if err != nil {
		return nil, err
	}
	if !jsonEquivalent(actualJSON, []byte(eventStreamBaseline)) {
		return nil, errors.New("Original TS behavior differs from the judge baseline; review the judge and source revision")
	}
	temp, err := os.MkdirTemp("", "portsmith-judge-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)
	if err := os.WriteFile(filepath.Join(temp, "go.mod"), []byte("module example.com/judge-check\n\ngo 1.24\n"), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(temp, "broken.go"), []byte(brokenOracleSample), 0o644); err != nil {
		return nil, err
	}
	if err := injectOracle(temp, cases); err != nil {
		return nil, err
	}
	build := executeGo(ctx, temp, []string{"-run", "^$"}, false, false)
	if !succeeded(build) {
		return nil, fmt.Errorf("Faulty sample did not compile and cannot validate the judge: %s", build.Log)
	}
	run := executeGo(ctx, temp, []string{"-run", "^TestPortsmithJudge"}, false, false)
	caught := testResults(run, "TestPortsmithJudge").Failed
	if (run.Code != nil && *run.Code == 0) || run.TimedOut || run.Truncated || caught < 1 {
		return nil, errors.New("Judge failed to detect a known defect and cannot be trusted")
	}
	oracleJSON, err := json.Marshal(cases)
	if err != nil {
		return nil, err
	}
	report := judgeCheckReport{
		Version:                         1,
		Verifier:                        VERIFIER_VERSION,
		At:                              time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Source:                          task.Files,
		Rules:                           task.RulesSHA256,
		BaselineCases:                   len(cases),
		KnownBrokenImplementationCaught: true,
		FailedTests:                     caught,
		OracleSHA256:                    Hash(oracleJSON),
	}
	if err := AtomicJSON(filepath.Join(root, "judge-check.json"), report); err != nil {
		return nil, err
	}
	return json.Marshal(report)
}

// jsonEquivalent compares two JSON documents after normalizing them through
// Go's generic decoder, so object member order and 1 vs 1.0 do not matter.
func jsonEquivalent(left, right []byte) bool {
	var a, b any
	if err := json.Unmarshal(left, &a); err != nil {
		return false
	}
	if err := json.Unmarshal(right, &b); err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}
