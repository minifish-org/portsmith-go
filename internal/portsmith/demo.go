// demo.go ports src/demo.ts: the optional offline event-stream replay.
//
// The TypeScript script prepared the bundled examples/event-stream directory,
// copied its reviewed reference-go files into the candidate and ran the full
// verifier without calling a model. The Go repository does not bundle that
// example tree, so this helper exposes the same workflow as a library call:
// point it at a directory with the same shape (source/ and reference-go/) and
// it performs an entirely offline replay. docs/USAGE.md documents the
// development workflow and the expected directory layout.
package portsmith

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// demoRevision is the revision label used by the bundled example so the task
// snapshot is reproducible. Callers may override it through DemoOptions.
const demoRevision = "pi-v0.87.1-f07218c4"

// demoReferenceFiles are the reviewed reference implementation files copied
// from the example's reference-go/ directory, in upstream order.
var demoReferenceFiles = []string{"event_stream.go", "event_stream_test.go", "NOTES.md"}

// DemoOptions configures an offline demo replay.
type DemoOptions struct {
	// Example is the directory containing source/ and reference-go/. It is
	// required; the Go product does not bundle the upstream example tree.
	Example string
	// Out is the destination task root. Empty selects
	// .portsmith/demo-<unix-milliseconds> below the current directory.
	Out string
	// Revision labels the task snapshot. Empty uses demoRevision.
	Revision string
}

// DemoTask replays the bundled event-stream example without any model calls.
// It prepares a fresh task snapshot, copies the example's reference Go files
// into the candidate, runs the isolated verifier and returns the prepared root
// together with the verification report. The caller decides how to render the
// report; this mirrors src/demo.ts, which exits non-zero unless the status is
// behavior_verified.
func DemoTask(ctx context.Context, options DemoOptions, progress func(string)) (string, Verification, error) {
	if strings.TrimSpace(options.Example) == "" {
		return "", Verification{}, errors.New("Demo requires --example, a directory containing source/ and reference-go/")
	}
	root := strings.TrimSpace(options.Out)
	if root == "" {
		root = filepath.Join(".portsmith", fmt.Sprintf("demo-%d", time.Now().UnixMilli()))
	}
	revision := options.Revision
	if revision == "" {
		revision = demoRevision
	}
	if _, err := PrepareTask(ctx, PrepareOptions{
		Source:   filepath.Join(options.Example, "source"),
		Out:      root,
		Revision: revision,
		Files: []string{
			"packages/ai/src/utils/event-stream.ts",
			"packages/ai/test/event-stream.test.ts",
		},
		Goal:    EVENT_GOAL,
		Example: "event-stream",
	}); err != nil {
		return root, Verification{}, err
	}
	for _, name := range demoReferenceFiles {
		content, err := os.ReadFile(filepath.Join(options.Example, "reference-go", name))
		if err != nil {
			return root, Verification{}, err
		}
		if err := WriteCandidate(root, name, content); err != nil {
			return root, Verification{}, err
		}
	}
	if progress != nil {
		progress(fmt.Sprintf("Replaying the existing example offline (no model calls): %s", root))
	}
	report, err := VerifyPort(ctx, root, false)
	if err != nil {
		return root, report, err
	}
	return root, report, nil
}
