package portsmith

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Phase is one named verification phase with its process result.
type Phase struct {
	Name   string        `json:"name"`
	Result ProcessResult `json:"result"`
}

// Verification is the verification report schema owned by src/verify.ts. It is
// declared here so diagnostics can format reports; the verification stage
// reuses this type instead of redefining it.
type Verification struct {
	Version          int     `json:"version"`
	Verifier         string  `json:"verifier"`
	At               string  `json:"at"`
	Fingerprint      string  `json:"fingerprint"`
	Status           string  `json:"status"`
	OracleCases      int     `json:"oracleCases"`
	Independent      bool    `json:"independent"`
	FullParityProven bool    `json:"fullParityProven"`
	Phases           []Phase `json:"phases"`
}

var goTestNoise = regexp.MustCompile(`^(=== RUN|=== PAUSE|=== CONT|--- PASS|PASS$)`)

// processDiagnostics filters `go test -json` noise out of a process log and
// keeps compiler/test diagnostics, matching upstream `processDiagnostics`.
func processDiagnostics(log string) string {
	lines := strings.Split(log, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		var event struct {
			Action     *string `json:"Action"`
			Output     *string `json:"Output"`
			Test       *string `json:"Test"`
			ImportPath *string `json:"ImportPath"`
			Package    *string `json:"Package"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil || event.Action == nil {
			out = append(out, line)
			continue
		}
		if event.Output != nil {
			if goTestNoise.MatchString(strings.TrimSpace(*event.Output)) {
				continue
			}
			out = append(out, strings.TrimRightFunc(*event.Output, unicode.IsSpace))
			continue
		}
		if *event.Action == "fail" || *event.Action == "build-fail" {
			name := ""
			switch {
			case event.Test != nil:
				name = *event.Test
			case event.ImportPath != nil:
				name = *event.ImportPath
			case event.Package != nil:
				name = *event.Package
			}
			out = append(out, "FAIL "+name)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// verificationDiagnostics renders the failed phases of a report, or the last
// phase when nothing failed, and caps the text like the upstream default.
func verificationDiagnostics(report Verification, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 24000
	}
	var failed []Phase
	for _, phase := range report.Phases {
		if phase.Result.Code == nil || *phase.Result.Code != 0 || phase.Result.TimedOut || phase.Result.Truncated {
			failed = append(failed, phase)
		}
	}
	phases := failed
	if len(phases) == 0 && len(report.Phases) > 0 {
		phases = report.Phases[len(report.Phases)-1:]
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Verification status: %s", report.Status)
	for _, phase := range phases {
		code := "null"
		if phase.Result.Code != nil {
			code = strconv.Itoa(*phase.Result.Code)
		}
		timedOut := ""
		if phase.Result.TimedOut {
			timedOut = ", timed out"
		}
		truncated := ""
		if phase.Result.Truncated {
			truncated = ", raw output truncated"
		}
		fmt.Fprintf(
			&builder,
			"\n%s (exit code %s%s%s):\n%s",
			phase.Name,
			code,
			timedOut,
			truncated,
			processDiagnostics(phase.Result.Log),
		)
	}
	text := builder.String()
	marker := "\n[Diagnostic summary truncated; see verification.json for full output]"
	if len(text) <= maxChars {
		return text
	}
	keep := maxChars - len(marker)
	if keep < 0 {
		keep = 0
	}
	return text[:keep] + marker
}
