package portsmith_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	ps "github.com/minifish-org/portsmith-go/internal/portsmith"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, root, name, value string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(value), 0600); e != nil {
		t.Fatal(e)
	}
}
func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func raw(t *testing.T, p string) []byte { t.Helper(); b, e := os.ReadFile(p); must(t, e); return b }
func digest(b []byte) string            { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var x map[string]any
	must(t, json.Unmarshal(b, &x))
	return x
}
func encode(t *testing.T, v any) string {
	t.Helper()
	b, e := json.Marshal(v)
	must(t, e)
	return string(b)
}
func rootOf(t *testing.T) string {
	t.Helper()
	p, e := os.Getwd()
	must(t, e)
	for {
		if _, e = os.Stat(filepath.Join(p, "go.mod")); e == nil {
			return p
		}
		n := filepath.Dir(p)
		if n == p {
			t.Fatal("no module root")
		}
		p = n
	}
}
func gitCmd(t *testing.T, root string, args ...string) string {
	t.Helper()
	all := append([]string{"-c", "user.name=Portsmith Judge", "-c", "user.email=judge@example.invalid", "-c", "commit.gpgsign=false"}, args...)
	c := exec.Command("git", all...)
	c.Dir = root
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v %s", args, e, b)
	}
	return strings.TrimSpace(string(b))
}

func TestPortsmithJudgeFilesAndProcess(t *testing.T) {
	if ps.Hash([]byte("hello")) != digest([]byte("hello")) {
		t.Fatal("SHA-256 mismatch")
	}
	d := t.TempDir()
	put(t, d, "safe/a.txt", "ok")
	n, e := ps.RelativeName("safe/a.txt")
	must(t, e)
	if n != "safe/a.txt" {
		t.Fatal(n)
	}
	for _, n := range []string{"../x", "/tmp/x", "a/../../x", "a\\b", ""} {
		if _, e = ps.RelativeName(n); e == nil {
			t.Fatalf("accepted unsafe name %q", n)
		}
	}
	_, e = ps.CheckedFile(d, "safe/a.txt")
	must(t, e)
	outside := t.TempDir()
	put(t, outside, "secret", "x")
	if e = os.Symlink(outside, filepath.Join(d, "link")); e == nil {
		if _, e = ps.CheckedFile(d, "link/secret"); e == nil {
			t.Fatal("followed source symlink")
		}
	}
	must(t, ps.AtomicJSON(filepath.Join(d, "state.json"), map[string]int{"v": 1}))
	if decode(t, raw(t, filepath.Join(d, "state.json")))["v"] != float64(1) {
		t.Fatal("atomic JSON write lost data")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	calls := 0
	must(t, ps.WithLock(ctx, d, func() error {
		calls++
		if e := ps.WithLock(ctx, d, func() error { calls++; return nil }); e == nil {
			t.Error("concurrent lock accepted")
		}
		return nil
	}))
	if calls != 1 {
		t.Fatal(calls)
	}
	must(t, ps.WithLock(ctx, d, func() error { return nil }))
	t.Setenv("PORTSMITH_API_KEY", "test-secret-never-inherit")
	env := ps.CleanEnv(false, false)
	if env["PORTSMITH_API_KEY"] != "" || env["CGO_ENABLED"] != "0" || env["GOPROXY"] != "off" || env["GOWORK"] != "off" {
		t.Fatal("unsafe verification environment", env)
	}
	r := ps.Execute(ctx, ps.ProcessOptions{Command: "go", Args: []string{"version"}, Cwd: d})
	if r.Code == nil || *r.Code != 0 || !strings.Contains(r.Log, "go version") {
		t.Fatalf("process result %+v", r)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	r = ps.Execute(cancelled, ps.ProcessOptions{Command: "go", Args: []string{"version"}, Cwd: d})
	if !r.Cancelled {
		t.Fatal("ignored cancellation")
	}
}

func TestPortsmithJudgeModelConfiguration(t *testing.T) {
	for _, n := range []string{"PORTSMITH_CONTEXT_WINDOW", "PORTSMITH_MAX_TOKENS"} {
		old, ok := os.LookupEnv(n)
		must(t, os.Unsetenv(n))
		t.Cleanup(func() {
			if ok {
				_ = os.Setenv(n, old)
			} else {
				_ = os.Unsetenv(n)
			}
		})
	}
	t.Setenv("PORTSMITH_BASE_URL", "https://api.deepseek.com/v1")
	t.Setenv("PORTSMITH_MODEL", "deepseek-flash")
	t.Setenv("PORTSMITH_API_KEY", "fixture-key")
	m, e := ps.ConfiguredModel()
	must(t, e)
	if m.ContextWindow != 1000000 || m.MaxTokens != 384000 || m.ID != "deepseek-flash" {
		t.Fatalf("lost catalog capacity: %+v", m)
	}
	if strings.Contains(encode(t, m), "fixture-key") {
		t.Fatal("serialized credential")
	}
	d := t.TempDir()
	put(t, d, "vars.env", "PORTSMITH_MODEL=must-not-override\nPORTSMITH_FIXTURE_VALUE='hello world'\n")
	t.Cleanup(func() { _ = os.Unsetenv("PORTSMITH_FIXTURE_VALUE") })
	must(t, ps.LoadLocalEnv(filepath.Join(d, "vars.env")))
	if os.Getenv("PORTSMITH_MODEL") != "deepseek-flash" || os.Getenv("PORTSMITH_FIXTURE_VALUE") != "hello world" {
		t.Fatal("dotenv precedence/quoting")
	}
	t.Setenv("PORTSMITH_MAX_TOKENS", "512")
	m, e = ps.ConfiguredModel()
	must(t, e)
	if m.MaxTokens != 512 {
		t.Fatal("override ignored")
	}
	t.Setenv("PORTSMITH_MAX_TOKENS", "-1")
	if _, e = ps.ConfiguredModel(); e == nil {
		t.Fatal("invalid output budget accepted")
	}
	t.Setenv("PORTSMITH_MAX_TOKENS", "512")
	t.Setenv("PORTSMITH_BASE_URL", "https://user:password@example.invalid/v1")
	if _, e = ps.ConfiguredModel(); e == nil {
		t.Fatal("credential-bearing URL accepted")
	}
}
