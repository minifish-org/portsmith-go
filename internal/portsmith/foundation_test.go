package portsmith

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testPut(t *testing.T, root, name, value string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHashMatchesSHA256(t *testing.T) {
	if got := Hash([]byte("hello")); got != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("hash mismatch: %s", got)
	}
	if Hash(nil) != Hash([]byte{}) {
		t.Fatal("nil and empty hashes differ")
	}
}

func TestRelativeNameRejectsUnsafePaths(t *testing.T) {
	if name, err := RelativeName("safe/a.txt"); err != nil || name != "safe/a.txt" {
		t.Fatalf("valid path rejected: %q %v", name, err)
	}
	for _, name := range []string{"", "../x", "/tmp/x", "a/../../x", "a\\b", "a//b", "./a", "a/./b", "a/\x00b"} {
		if _, err := RelativeName(name); err == nil {
			t.Fatalf("accepted unsafe name %q", name)
		}
	}
}

func TestCheckedFileRejectsSymlinksAndDirectories(t *testing.T) {
	root := t.TempDir()
	testPut(t, root, "safe/a.txt", "ok")
	if _, err := CheckedFile(root, "safe/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckedFile(root, "safe"); err == nil {
		t.Fatal("accepted a directory")
	}
	outside := t.TempDir()
	testPut(t, outside, "secret", "x")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err == nil {
		if _, err := CheckedFile(root, "link/secret"); err == nil {
			t.Fatal("followed a source symlink")
		}
	}
}

func TestAtomicJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "state.json")
	if err := AtomicJSON(file, map[string]any{"v": 1, "html": "<a>&"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "\\u003c") || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("unexpected encoding: %s", data)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["v"] != float64(1) || decoded["html"] != "<a>&" {
		t.Fatalf("round trip lost data: %v", decoded)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("temp file left behind: %v %v", entries, err)
	}
}

func TestWithLockExclusiveAndReleased(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	released := false
	if err := WithLock(ctx, dir, func() error {
		if err := WithLock(ctx, dir, func() error { return nil }); err == nil {
			t.Fatal("concurrent lock accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".lock")); !os.IsNotExist(err) {
		t.Fatal("lock not released")
	}
	if err := WithLock(ctx, dir, func() error { return os.ErrClosed }); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(filepath.Join(dir, ".lock")); !os.IsNotExist(err) {
		t.Fatal("lock not released after failure")
	}
	released = true
	_ = released
}

func TestSnapshotFilesDeterministicAndSorted(t *testing.T) {
	root := t.TempDir()
	testPut(t, root, "B.txt", "b")
	testPut(t, root, "a/inner.txt", "inner")
	testPut(t, root, "a.txt", "a")
	files, err := snapshotFiles(root, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "a/inner.txt,a.txt,B.txt" {
		t.Fatalf("unexpected order: %v", names)
	}
	if files[0].Name != "a/inner.txt" || files[0].SHA256 != Hash([]byte("inner")) {
		t.Fatal("snapshot hash mismatch")
	}
	// Symbolic links are rejected.
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "link.txt")); err == nil {
		if _, err := snapshotFiles(root, 0, 0, nil); err == nil {
			t.Fatal("snapshot accepted a symlink")
		}
	}
}

func TestCleanEnvAllowlist(t *testing.T) {
	t.Setenv("PORTSMITH_API_KEY", "secret-never-inherit")
	t.Setenv("GOMAXPROCS", "3")
	env := CleanEnv(false, false)
	if env["PORTSMITH_API_KEY"] != "" || env["GOMAXPROCS"] != "" {
		t.Fatalf("leaked environment: %v", env)
	}
	if env["CGO_ENABLED"] != "0" || env["GOPROXY"] != "off" || env["GOSUMDB"] != "off" || env["GOWORK"] != "off" || env["GOTOOLCHAIN"] != "local" {
		t.Fatalf("unexpected allowlist: %v", env)
	}
	race := CleanEnv(true, true)
	if race["CGO_ENABLED"] != "1" || race["GOPROXY"] != "https://proxy.golang.org" || race["GOSUMDB"] != "sum.golang.org" {
		t.Fatalf("unexpected race/download env: %v", race)
	}
}

func TestExecuteCapturesOutputAndExitCode(t *testing.T) {
	dir := t.TempDir()
	result := Execute(context.Background(), ProcessOptions{Command: "go", Args: []string{"version"}, Cwd: dir})
	if result.Code == nil || *result.Code != 0 || !strings.Contains(result.Log, "go version") {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.TimedOut || result.Truncated || result.Cancelled {
		t.Fatalf("unexpected flags: %+v", result)
	}
	bad := Execute(context.Background(), ProcessOptions{Command: "go", Args: []string{"--definitely-not-a-flag"}, Cwd: dir})
	if bad.Code == nil || *bad.Code == 0 {
		t.Fatalf("expected non-zero exit: %+v", bad)
	}
	missing := Execute(context.Background(), ProcessOptions{Command: "portsmith-missing-binary-xyz", Cwd: dir})
	if missing.Code != nil || missing.Log == "" {
		t.Fatalf("missing command result: %+v", missing)
	}
}

func TestExecuteCancellationAndPreAbort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper is unix-only")
	}
	dir := t.TempDir()
	pre, stop := context.WithCancel(context.Background())
	stop()
	got := Execute(pre, ProcessOptions{Command: "go", Args: []string{"version"}, Cwd: dir})
	if !got.Cancelled || got.Log != "Operation cancelled" || got.TimedOut {
		t.Fatalf("pre-abort result: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pending := make(chan ProcessResult, 1)
	go func() {
		pending <- Execute(ctx, ProcessOptions{Command: "sh", Args: []string{"-c", "sleep 30"}, Cwd: dir, Timeout: 30 * time.Second})
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case result := <-pending:
		if !result.Cancelled || result.TimedOut {
			t.Fatalf("cancel result: %+v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not terminate the child")
	}
}

func TestExecuteTimeoutAndTruncation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper is unix-only")
	}
	dir := t.TempDir()
	timed := Execute(context.Background(), ProcessOptions{Command: "sh", Args: []string{"-c", "sleep 30"}, Cwd: dir, Timeout: 150 * time.Millisecond})
	if !timed.TimedOut {
		t.Fatalf("timeout not reported: %+v", timed)
	}
	truncated := Execute(context.Background(), ProcessOptions{Command: "sh", Args: []string{"-c", "printf 'abcdefghij'"}, Cwd: dir, MaxLogChars: 4})
	if !truncated.Truncated || truncated.Log != "ghij" {
		t.Fatalf("truncation not reported: %+v", truncated)
	}
	unlimited := Execute(context.Background(), ProcessOptions{Command: "sh", Args: []string{"-c", "printf 'abcdefghij'"}, Cwd: dir})
	if unlimited.Truncated || unlimited.Log != "abcdefghij" {
		t.Fatalf("zero budget must be unlimited: %+v", unlimited)
	}
}

func TestProcessDiagnostics(t *testing.T) {
	log := strings.Join([]string{
		`{"Action":"output","Output":"=== RUN   TestFoo\n"}`,
		`{"Action":"output","Output":"--- PASS: TestFoo (0.00s)\n"}`,
		`{"Action":"output","Output":"    foo_test.go:10: real diagnostic\n"}`,
		`{"Action":"fail","Test":"TestFoo"}`,
		`not json`,
		`{"Action":"output","Output":"FAIL\n"}`,
	}, "\n")
	got := processDiagnostics(log)
	for _, want := range []string{"real diagnostic", "FAIL TestFoo", "not json"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	for _, noise := range []string{"=== RUN", "--- PASS"} {
		if strings.Contains(got, noise) {
			t.Fatalf("noise %q leaked into %q", noise, got)
		}
	}
}

func TestVerificationDiagnostics(t *testing.T) {
	report := Verification{
		Status: "compile_failed",
		Phases: []Phase{
			{Name: "compile", Result: ProcessResult{Code: intPointer(1), Log: `{"Action":"output","Output":"# pkg\nmain.go:3: undefined: x\n"}`}},
		},
	}
	got := verificationDiagnostics(report, 0)
	if !strings.Contains(got, "Verification status: compile_failed") || !strings.Contains(got, "undefined: x") {
		t.Fatalf("unexpected diagnostics: %s", got)
	}
	long := verificationDiagnostics(report, 40)
	if !strings.HasSuffix(long, "[Diagnostic summary truncated; see verification.json for full output]") {
		t.Fatalf("missing truncation marker: %q", long)
	}
}

func intPointer(value int) *int { return &value }

func envLookup(values map[string]string) lookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestModelLimits(t *testing.T) {
	known := &modelCapacity{ContextWindow: 1000000, MaxTokens: 384000}
	cases := []struct {
		name     string
		capacity *modelCapacity
		deepseek bool
		env      map[string]string
		wantCtx  int
		wantMax  int
	}{
		{"known defaults", known, true, nil, 1000000, 384000},
		{"unknown defaults", nil, false, nil, 32768, 4096},
		{"explicit output", known, true, map[string]string{"PORTSMITH_MAX_TOKENS": "65536"}, 1000000, 65536},
		{"small known", &modelCapacity{ContextWindow: 16384, MaxTokens: 4096}, true, nil, 16384, 4096},
		{"both explicit", known, true, map[string]string{"PORTSMITH_MAX_TOKENS": "65536", "PORTSMITH_CONTEXT_WINDOW": "262144"}, 262144, 65536},
	}
	for _, c := range cases {
		ctxWindow, maxTokens, err := modelLimits(c.capacity, c.deepseek, envLookup(c.env))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if ctxWindow != c.wantCtx || maxTokens != c.wantMax {
			t.Fatalf("%s: got %d/%d want %d/%d", c.name, ctxWindow, maxTokens, c.wantCtx, c.wantMax)
		}
	}
	for _, env := range []map[string]string{
		{"PORTSMITH_MAX_TOKENS": "NaN"},
		{"PORTSMITH_MAX_TOKENS": "0"},
		{"PORTSMITH_MAX_TOKENS": "1.5"},
		{"PORTSMITH_MAX_TOKENS": "500000"},
		{"PORTSMITH_CONTEXT_WINDOW": "8000000"},
		{"PORTSMITH_CONTEXT_WINDOW": "1024"},
		{"PORTSMITH_CONTEXT_WINDOW": "262144"},
		{"PORTSMITH_MAX_TOKENS": "131072", "PORTSMITH_CONTEXT_WINDOW": "131072"},
	} {
		if _, _, err := modelLimits(known, true, envLookup(env)); err == nil {
			t.Fatalf("accepted invalid limits %v", env)
		}
	}
}

func TestParseDotenvNodeSemantics(t *testing.T) {
	input := strings.Join([]string{
		"# comment",
		"PLAIN=value",
		"SPACED =  spaced  ",
		"QUOTED='hello world'",
		"DQUOTED=\"hello world\"",
		"INLINE=a # trailing",
		"HASH=a#b",
		"DHASH=\"a#b\"",
		"EMPTY=",
		"export KEY=val",
		`DQUOTE_ESC="a\nb"`,
		`SQUOTE_ESC='a\nb'`,
		"MULTI=\"line1\nline2\"",
		"AFTER=next",
	}, "\n")
	store := parseDotenv(input)
	expected := map[string]string{
		"PLAIN":      "value",
		"SPACED":     "spaced",
		"QUOTED":     "hello world",
		"DQUOTED":    "hello world",
		"INLINE":     "a",
		"HASH":       "a",
		"DHASH":      "a#b",
		"EMPTY":      "",
		"KEY":        "val",
		"DQUOTE_ESC": "a\nb",
		"SQUOTE_ESC": `a\nb`,
		"MULTI":      "line1\nline2",
		"AFTER":      "next",
	}
	for key, want := range expected {
		if got := store[key]; got != want {
			t.Fatalf("parseDotenv %s = %q want %q", key, got, want)
		}
	}
}

func TestLoadLocalEnvPrecedenceAndQuotes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "vars.env")
	testPut(t, dir, "vars.env", "PORTSMITH_MODEL=must-not-override\nPORTSMITH_FIXTURE_VALUE='hello world'\nCRLF=ok\r\n")
	t.Setenv("PORTSMITH_MODEL", "keep-me")
	t.Cleanup(func() {
		_ = os.Unsetenv("PORTSMITH_FIXTURE_VALUE")
		_ = os.Unsetenv("CRLF")
	})
	if err := LoadLocalEnv(file); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PORTSMITH_MODEL") != "keep-me" {
		t.Fatal("existing process value overridden")
	}
	if os.Getenv("PORTSMITH_FIXTURE_VALUE") != "hello world" || os.Getenv("CRLF") != "ok" {
		t.Fatalf("dotenv parsing failed: %q %q", os.Getenv("PORTSMITH_FIXTURE_VALUE"), os.Getenv("CRLF"))
	}
	if err := LoadLocalEnv(filepath.Join(dir, "missing.env")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestExecuteGoAndTestResults(t *testing.T) {
	module := t.TempDir()
	testPut(t, module, "go.mod", "module example.com/fixture\n\ngo 1.25.0\n")
	testPut(t, module, "fixture.go", "package fixture\n\nfunc Value() int { return 42 }\n")
	testPut(t, module, "fixture_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestFixture(t *testing.T) { if Value() != 42 { t.Fatal(\"bad\") } }\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := executeGo(ctx, module, []string{"-run", "TestFixture"}, false, false)
	if !succeeded(result) {
		t.Fatalf("executeGo failed: %+v", result)
	}
	counts := testResults(result, "Test")
	if counts.Passed != 1 || counts.Failed != 0 || len(counts.PassedNames) != 1 || counts.PassedNames[0] != "TestFixture" {
		t.Fatalf("unexpected test results: %+v", counts)
	}
	failing := testResults(ProcessResult{Code: result.Code, Log: strings.Join([]string{
		`{"Action":"pass","Test":"TestA"}`,
		`{"Action":"fail","Test":"TestB"}`,
		`{"Action":"skip","Test":"TestC"}`,
	}, "\n")}, "Test")
	if failing.Passed != 1 || failing.Failed != 1 || failing.Skipped != 1 {
		t.Fatalf("unexpected synthetic results: %+v", failing)
	}
	if succeeded(ProcessResult{Code: intPointer(0), Truncated: true}) {
		t.Fatal("truncated run reported success")
	}
}

func TestUnlimitedDefaults(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 2500; i++ {
		testPut(t, root, fmt.Sprintf("%d.txt", i), "fixture")
	}
	testPut(t, root, "large.txt", strings.Repeat("x", 2*1024*1024))
	files, err := snapshotFiles(root, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2501 {
		t.Fatalf("expected 2501 files, got %d", len(files))
	}
	if runtime.GOOS == "windows" {
		return
	}
	result := Execute(context.Background(), ProcessOptions{Command: "sh", Args: []string{"-c", "head -c 2097152 /dev/zero | tr '\\0' x"}, Cwd: root})
	if !succeeded(result) || result.Truncated || len(result.Log) != 2097152 {
		t.Fatalf("large output truncated: code=%v truncated=%v len=%d", result.Code, result.Truncated, len(result.Log))
	}
}

func TestConfiguredModelDeepSeekCapacity(t *testing.T) {
	for _, name := range []string{"PORTSMITH_CONTEXT_WINDOW", "PORTSMITH_MAX_TOKENS", "OMNI_BASE_URL", "OMNI_MODEL", "OMNI_API_KEY"} {
		old, ok := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if ok {
				_ = os.Setenv(name, old)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	t.Setenv("PORTSMITH_BASE_URL", "https://api.deepseek.com/v1")
	t.Setenv("PORTSMITH_MODEL", "deepseek-flash")
	t.Setenv("PORTSMITH_API_KEY", "fixture-key")
	model, err := ConfiguredModel()
	if err != nil {
		t.Fatal(err)
	}
	if model.ID != "deepseek-flash" || model.ContextWindow != 1000000 || model.MaxTokens != 384000 {
		t.Fatalf("lost catalog capacity: %+v", model)
	}
	if model.BaseURL != "https://api.deepseek.com/v1" {
		t.Fatalf("unexpected base URL: %s", model.BaseURL)
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "fixture-key") {
		t.Fatalf("serialized credential: %s", encoded)
	}
	t.Setenv("PORTSMITH_MAX_TOKENS", "512")
	model, err = ConfiguredModel()
	if err != nil || model.MaxTokens != 512 {
		t.Fatalf("explicit budget ignored: %+v %v", model, err)
	}
	t.Setenv("PORTSMITH_MAX_TOKENS", "-1")
	if _, err := ConfiguredModel(); err == nil {
		t.Fatal("invalid output budget accepted")
	}
	t.Setenv("PORTSMITH_MAX_TOKENS", "512")
	t.Setenv("PORTSMITH_BASE_URL", "https://user:password@example.invalid/v1")
	if _, err := ConfiguredModel(); err == nil {
		t.Fatal("credential-bearing URL accepted")
	}
	t.Setenv("PORTSMITH_BASE_URL", "https://api.deepseek.com/v1")
	t.Setenv("PORTSMITH_API_KEY", "   ")
	if _, err := ConfiguredModel(); err == nil {
		t.Fatal("blank DeepSeek key accepted")
	}
}
