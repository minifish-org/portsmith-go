package portsmith_test

import (
	"bytes"
	"context"
	"encoding/json"
	ps "github.com/minifish-org/portsmith-go/internal/portsmith"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPortsmithJudgeCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := ps.Main(context.Background(), []string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatal("help failed", code, stderr.String())
	}
	for _, s := range []string{"analyze", "plan", "prepare", "run", "verify", "judge-check", "status", "next", "accept", "migrate"} {
		if !strings.Contains(stdout.String(), s) {
			t.Errorf("help missing %s", s)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if ps.Main(context.Background(), []string{"unknown"}, &stdout, &stderr) == 0 {
		t.Fatal("unknown command succeeded")
	}
	d := t.TempDir()
	copyFixture(t, "testdata/analysis/input", d)
	out := filepath.Join(t.TempDir(), "report.json")
	stdout.Reset()
	stderr.Reset()
	if code := ps.Main(context.Background(), []string{"analyze", "--source", d, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatal(code, stderr.String())
	}
	if decode(t, raw(t, out))["version"] != float64(1) {
		t.Fatal("invalid CLI report")
	}
}

func TestPortsmithJudgeStandaloneAndCrossBuild(t *testing.T) {
	root := rootOf(t)
	binary := filepath.Join(t.TempDir(), "portsmith")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	cmd := exec.Command("go", "build", "-mod=readonly", "-o", binary, "./cmd/portsmith")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("native build: %v %s", e, b)
	}
	// Hide every installed runtime. help/analyze need neither Go nor Git nor Node.
	cmd = exec.Command(binary, "--help")
	cmd.Env = []string{"PATH=" + t.TempDir(), "HOME=" + t.TempDir()}
	if b, e := cmd.CombinedOutput(); e != nil || !strings.Contains(string(b), "analyze") {
		t.Fatalf("standalone help: %v %s", e, b)
	}
	source := t.TempDir()
	copyFixture(t, "testdata/analysis/input", source)
	out := filepath.Join(t.TempDir(), "analysis.json")
	cmd = exec.Command(binary, "analyze", "--source", source, "--out", out)
	cmd.Env = []string{"PATH=" + t.TempDir(), "HOME=" + t.TempDir()}
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("analyze needs hidden runtime: %v %s", e, b)
	}
	if decode(t, raw(t, out))["version"] != float64(1) {
		t.Fatal("standalone analyze did not write a report")
	}
	for _, target := range [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}, {"windows", "amd64"}} {
		cmd = exec.Command("go", "build", "-mod=readonly", "-o", filepath.Join(t.TempDir(), "portsmith"), "./cmd/portsmith")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target[0], "GOARCH="+target[1])
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("cross-build %v: %v %s", target, e, b)
		}
	}
}

func TestPortsmithJudgeSourceCoverageAndPithDependency(t *testing.T) {
	root := rootOf(t)
	var records []struct {
		Source, SHA256, Status, Notes string
		GoFiles                       []string
	}
	must(t, json.Unmarshal(raw(t, filepath.Join(root, "docs/source-map.json")), &records))
	expected := decode(t, raw(t, "testdata/source-hashes.json"))
	seen := map[string]bool{}
	for _, r := range records {
		if seen[r.Source] {
			t.Fatal("duplicate mapping", r.Source)
		}
		seen[r.Source] = true
		if expected[r.Source] != r.SHA256 || len(r.GoFiles) == 0 || (r.Status != "ported" && r.Status != "adapted") {
			t.Fatal("incomplete source mapping", r)
		}
		for _, f := range r.GoFiles {
			if !strings.HasSuffix(f, ".go") {
				t.Fatal("map must name Go files", f)
			}
			if _, e := os.Stat(filepath.Join(root, f)); e != nil {
				t.Fatal(e)
			}
		}
		if r.Status == "adapted" && r.Notes == "" {
			t.Fatal("undocumented adaptation")
		}
	}
	if len(records) != len(expected) {
		t.Fatalf("mapped %d of %d source files", len(records), len(expected))
	}
	agent := string(raw(t, filepath.Join(root, "internal/portsmith/agent.go")))
	if !strings.Contains(agent, "github.com/minifish-org/pith/packages/coding-agent") || !strings.Contains(agent, "CreateAgentSession") {
		t.Fatal("agent must embed Pith coding-agent")
	}
	mod := string(raw(t, filepath.Join(root, "go.mod")))
	if !strings.Contains(mod, "github.com/minifish-org/pith v0.0.0-20261003091049-9ef56f7b1a53") || strings.Contains(mod, "replace ") {
		t.Fatal("unpinned or local-only Pith dependency")
	}
	for _, name := range []string{"docs/USAGE.md", "docs/COMPATIBILITY.md", "docs/THIRD_PARTY.md"} {
		if len(raw(t, filepath.Join(root, name))) < 100 {
			t.Fatal("missing delivery documentation", name)
		}
	}
}
