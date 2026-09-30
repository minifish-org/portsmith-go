// verification_test.go adds candidate-owned coverage for the isolated
// verifier, the live TypeScript EventStream oracle and the judge negative
// control. The embedded source below matches the frozen upstream fixture so
// the oracle is exercised against real TypeScript, not a constant golden.
package portsmith

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// eventStreamSource is the supplied original TypeScript module. The oracle must
// execute it, so a behavior mutation must change the returned trace.
const eventStreamSource = `import type { AssistantMessage, AssistantMessageEvent } from "../types.ts";

class FifoQueue<T> {
	private incoming: T[] = [];
	private outgoing: T[] = [];

	get length(): number {
		return this.incoming.length + this.outgoing.length;
	}

	enqueue(value: T): void {
		this.incoming.push(value);
	}

	dequeue(): T | undefined {
		if (this.outgoing.length === 0) {
			while (this.incoming.length > 0) {
				this.outgoing.push(this.incoming.pop()!);
			}
		}
		return this.outgoing.pop();
	}
}

// Generic event stream class for async iteration
export class EventStream<T, R = T> implements AsyncIterable<T> {
	private queue = new FifoQueue<T>();
	private waiting = new FifoQueue<(value: IteratorResult<T>) => void>();
	private done = false;
	private finalResultPromise: Promise<R>;
	private resolveFinalResult!: (result: R) => void;
	private isComplete: (event: T) => boolean;
	private extractResult: (event: T) => R;

	constructor(isComplete: (event: T) => boolean, extractResult: (event: T) => R) {
		this.isComplete = isComplete;
		this.extractResult = extractResult;
		this.finalResultPromise = new Promise((resolve) => {
			this.resolveFinalResult = resolve;
		});
	}

	push(event: T): void {
		if (this.done) return;

		if (this.isComplete(event)) {
			this.done = true;
			this.resolveFinalResult(this.extractResult(event));
		}

		// Deliver to waiting consumer or queue it
		const waiter = this.waiting.dequeue();
		if (waiter) {
			waiter({ value: event, done: false });
		} else {
			this.queue.enqueue(event);
		}
	}

	end(result?: R): void {
		this.done = true;
		if (result !== undefined) {
			this.resolveFinalResult(result);
		}
		// Notify all waiting consumers that we're done
		while (this.waiting.length > 0) {
			const waiter = this.waiting.dequeue()!;
			waiter({ value: undefined as any, done: true });
		}
	}

	async *[Symbol.asyncIterator](): AsyncIterator<T> {
		while (true) {
			if (this.queue.length > 0) {
				yield this.queue.dequeue()!;
			} else if (this.done) {
				return;
			} else {
				const result = await new Promise<IteratorResult<T>>((resolve) => this.waiting.enqueue(resolve));
				if (result.done) return;
				yield result.value;
			}
		}
	}

	result(): Promise<R> {
		return this.finalResultPromise;
	}
}

export class AssistantMessageEventStream extends EventStream<AssistantMessageEvent, AssistantMessage> {
	constructor() {
		super(
			(event) => event.type === "done" || event.type === "error",
			(event) => {
				if (event.type === "done") {
					return event.message;
				} else if (event.type === "error") {
					return event.error;
				}
				throw new Error("Unexpected event type for final result");
			},
		);
	}
}

/** Factory function for AssistantMessageEventStream (for use in extensions) */
export function createAssistantMessageEventStream(): AssistantMessageEventStream {
	return new AssistantMessageEventStream();
}
`

const workingEventStreamGo = `package port

import (
	"context"
	"sync"
)

type StreamItem[T any] struct {
	Value T
	Done  bool
}

type EventStream[T any, R any] struct {
	mu         sync.Mutex
	queue      []T
	waiters    []chan StreamItem[T]
	done       bool
	resolved   bool
	result     R
	resultCh   chan struct{}
	isComplete func(T) bool
	extract    func(T) R
}

func NewEventStream[T any, R any](isComplete func(T) bool, extractResult func(T) R) *EventStream[T, R] {
	return &EventStream[T, R]{isComplete: isComplete, extract: extractResult, resultCh: make(chan struct{})}
}

func (s *EventStream[T, R]) Push(event T) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	if s.isComplete(event) {
		s.done = true
		if !s.resolved {
			s.resolved = true
			s.result = s.extract(event)
			close(s.resultCh)
		}
	}
	if len(s.waiters) > 0 {
		waiter := s.waiters[0]
		s.waiters = s.waiters[1:]
		s.mu.Unlock()
		waiter <- StreamItem[T]{Value: event}
		return
	}
	s.queue = append(s.queue, event)
	s.mu.Unlock()
}

func (s *EventStream[T, R]) End(result *R) {
	s.mu.Lock()
	s.done = true
	if result != nil && !s.resolved {
		s.resolved = true
		s.result = *result
		close(s.resultCh)
	}
	waiters := s.waiters
	s.waiters = nil
	s.mu.Unlock()
	for _, waiter := range waiters {
		waiter <- StreamItem[T]{Done: true}
	}
}

func (s *EventStream[T, R]) Next() <-chan StreamItem[T] {
	out := make(chan StreamItem[T], 1)
	s.mu.Lock()
	if len(s.queue) > 0 {
		value := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		out <- StreamItem[T]{Value: value}
		return out
	}
	if s.done {
		s.mu.Unlock()
		out <- StreamItem[T]{Done: true}
		return out
	}
	s.waiters = append(s.waiters, out)
	s.mu.Unlock()
	return out
}

func (s *EventStream[T, R]) Result(ctx context.Context) (R, error) {
	select {
	case <-s.resultCh:
		s.mu.Lock()
		value := s.result
		s.mu.Unlock()
		return value, nil
	case <-ctx.Done():
		var zero R
		return zero, ctx.Err()
	}
}
`

const workingEventStreamTest = `package port

import (
	"context"
	"testing"
)

func TestEventStreamCandidateBasics(t *testing.T) {
	s := NewEventStream[int, int](func(n int) bool { return n == 2 }, func(n int) int { return n * 10 })
	s.Push(1)
	if item := <-s.Next(); item.Done || item.Value != 1 {
		t.Fatalf("unexpected item: %+v", item)
	}
	s.Push(2)
	if item := <-s.Next(); item.Done || item.Value != 2 {
		t.Fatalf("unexpected item: %+v", item)
	}
	result, err := s.Result(context.Background())
	if err != nil || result != 20 {
		t.Fatalf("result=%v err=%v", result, err)
	}
}
`

const selfTestGo = "package port\nimport \"testing\"\nfunc TestCandidateValue(t *testing.T){if Value()!=42{t.Fatal(\"wrong\")}}\n"

const valueJudgeGo = "package port\nimport \"testing\"\nfunc TestPortsmithJudgeValue(t *testing.T){if Value()!=42{t.Fatal(\"wrong\")}}\n"

// verificationFixture prepares a small custom-judge task whose reference is
// value.ts and whose independent judge requires Value()==42.
func verificationFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	testPut(t, source, "LICENSE", "fixture license\n")
	testPut(t, source, "value.ts", "export function Value(){ return 42; }\n")
	judge := filepath.Join(dir, "judge")
	testPut(t, judge, "value_judge_test.go", valueJudgeGo)
	root, err := PrepareTask(context.Background(), PrepareOptions{
		Source:             source,
		Out:                filepath.Join(dir, "task"),
		Files:              []string{"value.ts"},
		Revision:           "fixture",
		Goal:               "Port Value returning 42",
		GoMod:              workspaceMod(t, dir),
		Judge:              judge,
		RequiredJudgeTests: []string{"TestPortsmithJudgeValue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// eventStreamFixture prepares the built-in EventStream example with a working
// candidate so the full judge-check and oracle injection path runs.
func eventStreamFixture(t *testing.T) string {
	return eventStreamFixtureWithSource(t, eventStreamSource)
}

// eventStreamFixtureWithSource prepares the example task from an arbitrary
// source snapshot, which lets tests exercise baseline drift.
func eventStreamFixtureWithSource(t *testing.T, eventSource string) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	testPut(t, source, "LICENSE", "fixture license\n")
	testPut(t, source, "packages/ai/src/utils/event-stream.ts", eventSource)
	root, err := PrepareTask(context.Background(), PrepareOptions{
		Source:   source,
		Out:      filepath.Join(dir, "task"),
		Files:    []string{"packages/ai/src/utils/event-stream.ts"},
		Revision: "fixture",
		Goal:     "Port the EventStream example",
		Example:  "event-stream",
		GoMod:    workspaceMod(t, dir),
		Initial: []File{
			{Name: "event_stream.go", Data: []byte(workingEventStreamGo)},
			{Name: "event_stream_test.go", Data: []byte(workingEventStreamTest)},
		},
		ModuleTask:    true,
		WritableFiles: []string{"event_stream.go", "event_stream_test.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestVerifyPortIndependentJudgeAndReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := verificationFixture(t)
	if err := WriteCandidate(root, "value.go", []byte("package port\nfunc Value() int { return 42 }\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value_test.go", []byte(selfTestGo)); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyPort(ctx, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != 1 || report.Verifier != VERIFIER_VERSION || report.Status != "behavior_verified" || !report.Independent || report.FullParityProven {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.OracleCases != 0 {
		t.Fatalf("custom judge must not claim oracle cases: %+v", report)
	}
	current, err := CurrentVerification(root)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || !current.Current {
		t.Fatalf("fresh receipt rejected: %+v", current)
	}
	if err := WriteCandidate(root, "value.go", []byte("package port\nfunc Value() int { return 41 }\n")); err != nil {
		t.Fatal(err)
	}
	current, err = CurrentVerification(root)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.Current {
		t.Fatalf("stale receipt trusted: %+v", current)
	}
}

func TestVerifyPortRejectsReservedAndForged(t *testing.T) {
	ctx := context.Background()
	root := verificationFixture(t)
	if err := WriteCandidate(root, "value.go", []byte("package port\nfunc Value() int { return 42 }\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value_test.go", []byte(selfTestGo)); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "forged_test.go", []byte("package port\nimport \"testing\"\nfunc TestPortsmithJudgeForged(t *testing.T){}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPort(ctx, root, false); err == nil {
		t.Fatal("forged independent test accepted")
	}
}

func TestVerifyPortRejectsSkippedAndMissingCandidateTests(t *testing.T) {
	ctx := context.Background()
	root := verificationFixture(t)
	if err := WriteCandidate(root, "value.go", []byte("package port\nfunc Value() int { return 42 }\n")); err != nil {
		t.Fatal(err)
	}
	if report, err := VerifyPort(ctx, root, false); err != nil {
		t.Fatal(err)
	} else if report.Status == "behavior_verified" {
		t.Fatalf("candidate without tests verified: %+v", report)
	}
	root = verificationFixture(t)
	if err := WriteCandidate(root, "value.go", []byte("package port\nfunc Value() int { return 42 }\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value_test.go", []byte("package port\nimport \"testing\"\nfunc TestSkipped(t *testing.T){t.Skip(\"not evidence\")}\n")); err != nil {
		t.Fatal(err)
	}
	if report, err := VerifyPort(ctx, root, false); err != nil {
		t.Fatal(err)
	} else if report.Status == "behavior_verified" {
		t.Fatalf("skipped candidate tests verified: %+v", report)
	}
}

func TestEventStreamOracleLiveAndMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	testPut(t, root, "references/packages/ai/src/utils/event-stream.ts", eventStreamSource)
	first, err := EventStreamOracle(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	var cases []oracleCase
	if err := json.Unmarshal(first, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 7 {
		t.Fatalf("expected seven scenarios, got %d", len(cases))
	}
	if len(cases[0].Expected) != 5 || len(cases[6].Expected) != 2 {
		t.Fatalf("unexpected scenario traces: %+v", cases)
	}
	if !strings.Contains(eventStreamSource, "this.extractResult(event)") {
		t.Fatal("mutation target missing")
	}
	mutated := strings.ReplaceAll(eventStreamSource, "this.extractResult(event)", "(this.extractResult(event) as any) + 100")
	testPut(t, root, "references/packages/ai/src/utils/event-stream.ts", mutated)
	second, err := EventStreamOracle(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	var changed []oracleCase
	if err := json.Unmarshal(second, &changed); err != nil {
		t.Fatal(err)
	}
	if len(changed) != 7 {
		t.Fatalf("mutated source lost scenarios: %d", len(changed))
	}
	if string(first) == string(second) {
		t.Fatal("oracle returned a constant golden instead of executing TypeScript")
	}
	if len(changed[0].Expected) == 0 {
		t.Fatal("mutated source produced no trace")
	}
}

func TestEventStreamOracleHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	testPut(t, root, "references/packages/ai/src/utils/event-stream.ts", eventStreamSource)
	if _, err := EventStreamOracle(ctx, root); err == nil {
		t.Fatal("cancelled oracle accepted")
	}
}

func TestJudgeCheckKnownBrokenNegativeControl(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	root := eventStreamFixture(t)
	report, err := VerifyPort(ctx, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "behavior_verified" || !report.Independent {
		t.Fatalf("example task not verified: %+v", report)
	}
	if report.OracleCases != 7 {
		t.Fatalf("expected seven oracle cases: %+v", report)
	}
	if _, err := CurrentVerification(root); err != nil {
		t.Fatal(err)
	}
}

func TestJudgeCheckDetectsSourceDrift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	drifted := strings.ReplaceAll(eventStreamSource, "this.extractResult(event)", "(this.extractResult(event) as any) + 1")
	root := eventStreamFixtureWithSource(t, drifted)
	if _, err := JudgeCheck(ctx, root); err == nil {
		t.Fatal("baseline drift silently accepted")
	}
}

func TestVerifyPortRequiresRequiredJudgeNames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	testPut(t, source, "LICENSE", "fixture license\n")
	testPut(t, source, "value.ts", "export function Value(){ return 42; }\n")
	judge := filepath.Join(dir, "judge")
	testPut(t, judge, "value_judge_test.go", valueJudgeGo)
	root, err := PrepareTask(context.Background(), PrepareOptions{
		Source:             source,
		Out:                filepath.Join(dir, "task"),
		Files:              []string{"value.ts"},
		Revision:           "fixture",
		Goal:               "Port Value returning 42",
		GoMod:              workspaceMod(t, dir),
		Judge:              judge,
		RequiredJudgeTests: []string{"TestPortsmithJudgeValue", "TestPortsmithJudgeNeverRuns"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value.go", []byte("package port\nfunc Value() int { return 42 }\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value_test.go", []byte(selfTestGo)); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyPort(ctx, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status == "behavior_verified" {
		t.Fatalf("missing required judge test was accepted: %+v", report)
	}
}

func TestVerifyPortWritesVerifierErrorPhase(t *testing.T) {
	ctx := context.Background()
	root := verificationFixture(t)
	if err := WriteCandidate(root, "value.go", []byte("package port\nfunc Value() int { return 42 }\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value_test.go", []byte(selfTestGo)); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "forged_test.go", []byte("package port\nimport \"testing\"\nfunc TestPortsmithJudgeForged(t *testing.T){}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPort(ctx, root, false); err == nil {
		t.Fatal("forged test accepted")
	}
	raw, err := readJSON[Verification](root, "verification.json")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, phase := range raw.Phases {
		if phase.Name == "verifier-error" {
			found = true
		}
	}
	if !found {
		t.Fatalf("verifier-error phase missing: %+v", raw.Phases)
	}
}

func TestAcceptTaskRequiresBehaviorVerification(t *testing.T) {
	root := verificationFixture(t)
	if _, err := AcceptTask(root, filepath.Join(t.TempDir(), "export")); err == nil {
		t.Fatal("acceptance without verification accepted")
	}
}

func TestAcceptTaskExportsVerifiedCandidate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := verificationFixture(t)
	if err := WriteCandidate(root, "value.go", []byte("package port\nfunc Value() int { return 42 }\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value_test.go", []byte(selfTestGo)); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyPort(ctx, root, false)
	if err != nil || report.Status != "behavior_verified" {
		t.Fatalf("verify failed: %+v %v", report, err)
	}
	destination, err := AcceptTask(root, filepath.Join(t.TempDir(), "export"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "PORTSMITH-RECEIPT.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "acceptance.json")); err != nil {
		t.Fatal(err)
	}
}
