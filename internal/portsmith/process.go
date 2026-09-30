package portsmith

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ProcessOptions configures a child process invocation (upstream `execute`
// arguments). Timeout and MaxLogChars of zero mean unlimited.
type ProcessOptions struct {
	Command        string
	Args           []string
	Cwd            string
	Timeout        time.Duration
	Download, Race bool
	MaxLogChars    int
}

// ProcessResult is the upstream ProcessResult schema.
type ProcessResult struct {
	Code      *int   `json:"code"`
	TimedOut  bool   `json:"timedOut"`
	Truncated bool   `json:"truncated"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Log       string `json:"log"`
}

// CleanEnv returns the verification allowlist. Provider credentials and
// unrelated process variables never reach a child process.
func CleanEnv(download, race bool) map[string]string {
	env := make(map[string]string, 10)
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "SystemRoot"} {
		if value := os.Getenv(name); value != "" {
			env[name] = value
		}
	}
	env["GOTOOLCHAIN"] = "local"
	if race {
		env["CGO_ENABLED"] = "1"
	} else {
		env["CGO_ENABLED"] = "0"
	}
	env["GOENV"] = "off"
	env["GOWORK"] = "off"
	if download {
		env["GOPROXY"] = "https://proxy.golang.org"
		env["GOSUMDB"] = "sum.golang.org"
	} else {
		env["GOPROXY"] = "off"
		env["GOSUMDB"] = "off"
	}
	return env
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out
}

// logRecorder accumulates child output and keeps only the trailing
// MaxLogChars runes when a positive budget is configured.
type logRecorder struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

func (r *logRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if r.max > 0 && utf8.RuneCount(r.buf) > r.max {
		start := len(r.buf)
		count := 0
		for i := len(r.buf); i > 0 && count < r.max; count++ {
			_, size := utf8.DecodeLastRune(r.buf[:i])
			i -= size
			start = i
		}
		r.buf = append([]byte(nil), r.buf[start:]...)
		r.truncated = true
	}
	return len(p), nil
}

func (r *logRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// Execute runs a child process with the clean environment, merges stdout and
// stderr into one log, and reports timeout, truncation and cancellation
// separately. Cancellation kills the whole process group where supported.
func Execute(ctx context.Context, opts ProcessOptions) ProcessResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return ProcessResult{Cancelled: true, Log: "Operation cancelled"}
	}
	cmd := exec.Command(opts.Command, opts.Args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = envList(CleanEnv(opts.Download, opts.Race))
	setProcessGroup(cmd)
	recorder := &logRecorder{max: opts.MaxLogChars}
	cmd.Stdout = recorder
	cmd.Stderr = recorder

	var mu sync.Mutex
	var timedOut, cancelled, exited bool
	finish := make(chan struct{})

	kill := func() {
		if cmd.Process != nil {
			killProcess(cmd)
		}
	}

	if err := cmd.Start(); err != nil {
		recorder.Write([]byte(err.Error()))
		return ProcessResult{Log: recorder.String(), Truncated: recorder.truncated}
	}

	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			mu.Lock()
			if !exited {
				cancelled = true
			}
			mu.Unlock()
			kill()
		case <-finish:
		}
	}()

	var timer *time.Timer
	if opts.Timeout > 0 {
		timer = time.AfterFunc(opts.Timeout, func() {
			mu.Lock()
			if !exited {
				timedOut = true
			}
			mu.Unlock()
			kill()
		})
	}

	waitErr := cmd.Wait()
	mu.Lock()
	exited = true
	if timer != nil {
		timer.Stop()
	}
	state := cmd.ProcessState
	mu.Unlock()
	close(finish)
	<-watcherDone

	mu.Lock()
	result := ProcessResult{
		TimedOut:  timedOut,
		Truncated: recorder.truncated,
		Cancelled: cancelled,
		Log:       recorder.String(),
	}
	mu.Unlock()

	if waitErr == nil {
		code := 0
		result.Code = &code
	} else if state != nil && state.Exited() {
		code := state.ExitCode()
		result.Code = &code
	}
	return result
}

// executeGo runs `go test -json` in directory with the pinned read-only
// module mode, matching upstream `executeGo`.
func executeGo(ctx context.Context, directory string, args []string, download, race bool) ProcessResult {
	goArgs := []string{"test", "-json", "-mod=readonly", "-count=1", "-timeout=0"}
	if race {
		goArgs = append(goArgs, "-race")
	}
	goArgs = append(goArgs, args...)
	goArgs = append(goArgs, "./...")
	return Execute(ctx, ProcessOptions{
		Command:  "go",
		Args:     goArgs,
		Cwd:      directory,
		Download: download,
		Race:     race,
	})
}

// GoTestResults summarizes parsed `go test -json` events.
type GoTestResults struct {
	Passed      int
	Failed      int
	Skipped     int
	PassedNames []string
}

// testResults parses `go test -json` output and counts tests whose name starts
// with prefix, matching upstream `testResults`.
func testResults(result ProcessResult, prefix string) GoTestResults {
	type event struct {
		Test   string `json:"Test"`
		Action string `json:"Action"`
	}
	counts := GoTestResults{}
	seen := map[string]bool{}
	for _, line := range strings.Split(result.Log, "\n") {
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e.Test == "" || !strings.HasPrefix(e.Test, prefix) {
			continue
		}
		switch e.Action {
		case "pass":
			counts.Passed++
			if !seen[e.Test] {
				seen[e.Test] = true
				counts.PassedNames = append(counts.PassedNames, e.Test)
			}
		case "fail":
			counts.Failed++
		case "skip":
			counts.Skipped++
		}
	}
	return counts
}

// succeeded reports whether a process finished cleanly.
func succeeded(result ProcessResult) bool {
	return result.Code != nil && *result.Code == 0 && !result.TimedOut && !result.Truncated && !result.Cancelled
}
