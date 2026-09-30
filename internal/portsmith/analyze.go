// analyze.go ports src/analyze.ts: a deterministic, model-free TypeScript file
// graph. Filesystem walking, hashing and strongly connected components are Go;
// AST traversal and module resolution run inside the embedded TypeScript
// compiler through typescript.go.
package portsmith

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ImportEdge is one resolved, unresolved, external or computed import.
type ImportEdge struct {
	Specifier string `json:"specifier"`
	Kind      string `json:"kind"`
	Target    string `json:"target,omitempty"`
	TypeOnly  bool   `json:"typeOnly"`
	Line      int    `json:"line"`
}

// SourceFile is one analyzed TypeScript-family source file.
type SourceFile struct {
	Path    string       `json:"path"`
	SHA256  string       `json:"sha256"`
	Lines   int          `json:"lines"`
	Test    bool         `json:"test"`
	Exports []string     `json:"exports"`
	Imports []ImportEdge `json:"imports"`
}

// AnalysisConfig records a deterministic hash of a package.json or tsconfig.
type AnalysisConfig struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Analysis is the version-1 report returned by Analyze.
type Analysis struct {
	Version  int              `json:"version"`
	Source   string           `json:"source"`
	Files    []SourceFile     `json:"files"`
	Cycles   [][]string       `json:"cycles"`
	Warnings []string         `json:"warnings"`
	Configs  []AnalysisConfig `json:"configs"`
}

var (
	sourceFilePattern = regexp.MustCompile(`\.[cm]?[jt]sx?$`)
	tsconfigPattern   = regexp.MustCompile(`^tsconfig.*\.json$`)
	testFilePattern   = regexp.MustCompile(`(^|/)(__tests__|tests?)(/|$)|\.(test|spec)\.`)
)

var skippedDirectories = map[string]bool{
	"node_modules": true,
	"dist":         true,
	"build":        true,
	"coverage":     true,
	"vendor":       true,
}

type sourceMeta struct {
	sha256 string
	lines  int
	test   bool
}

type sourceLayout struct {
	names    []string
	configs  []AnalysisConfig
	packages []string
	warnings []string
	meta     map[string]sourceMeta
}

// components implements Tarjan's strongly connected components in the same
// dependency-first order as src/analyze.ts. Each returned group is sorted.
func components(nodes []string, edges map[string][]string) [][]string {
	next := 0
	indexes := map[string]int{}
	low := map[string]int{}
	var stack []string
	active := map[string]bool{}
	var groups [][]string

	var visit func(n string)
	visit = func(n string) {
		indexes[n] = next
		low[n] = next
		next++
		stack = append(stack, n)
		active[n] = true
		for _, d := range edges[n] {
			if _, seen := indexes[d]; !seen {
				visit(d)
				if low[d] < low[n] {
					low[n] = low[d]
				}
			} else if active[d] {
				if indexes[d] < low[n] {
					low[n] = indexes[d]
				}
			}
		}
		if low[n] == indexes[n] {
			var group []string
			for {
				d := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				active[d] = false
				group = append(group, d)
				if d == n {
					break
				}
			}
			sort.Strings(group)
			groups = append(groups, group)
		}
	}

	sorted := append([]string(nil), nodes...)
	sort.Strings(sorted)
	for _, n := range sorted {
		if _, seen := indexes[n]; !seen {
			visit(n)
		}
	}
	return groups
}

func countSourceLines(text string) int {
	if text == "" {
		return 0
	}
	newlines := strings.Count(text, "\n")
	if strings.HasSuffix(text, "\n") {
		return newlines
	}
	return newlines + 1
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// collectSourceFiles walks the source tree exactly as analyze.ts does:
// hidden/build/dependency trees are skipped, symbolic links are recorded as
// warnings and skipped, and only TypeScript-family files and configs are kept.
func collectSourceFiles(ctx context.Context, source string) (*sourceLayout, error) {
	layout := &sourceLayout{meta: map[string]sourceMeta{}}
	var walk func(relative string) error
	walk = func(relative string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		directory := filepath.Join(source, filepath.FromSlash(relative))
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if skippedDirectories[name] || strings.HasPrefix(name, ".") {
				continue
			}
			childRelative := path.Join(relative, name)
			childAbsolute := filepath.Join(source, filepath.FromSlash(childRelative))
			if entry.Type()&os.ModeSymlink != 0 {
				layout.warnings = append(layout.warnings, "Skipped symbolic link: "+childRelative)
				continue
			}
			if entry.IsDir() {
				if err := walk(childRelative); err != nil {
					return err
				}
				continue
			}
			if sourceFilePattern.MatchString(name) {
				data, err := os.ReadFile(childAbsolute)
				if err != nil {
					return err
				}
				text := string(data)
				layout.names = append(layout.names, childRelative)
				layout.meta[childRelative] = sourceMeta{
					sha256: Hash(data),
					lines:  countSourceLines(text),
					test:   testFilePattern.MatchString(childRelative),
				}
				continue
			}
			if name == "package.json" || tsconfigPattern.MatchString(name) {
				data, err := os.ReadFile(childAbsolute)
				if err != nil {
					return err
				}
				layout.configs = append(layout.configs, AnalysisConfig{Path: childRelative, SHA256: Hash(data)})
				if name == "package.json" {
					var pkg struct {
						Name string `json:"name"`
					}
					if err := json.Unmarshal(data, &pkg); err != nil {
						layout.warnings = append(layout.warnings, "Unable to parse: "+childRelative)
					} else if pkg.Name != "" {
						layout.packages = append(layout.packages, pkg.Name)
					}
				}
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	sort.Strings(layout.names)
	sort.SliceStable(layout.configs, func(i, j int) bool {
		return layout.configs[i].Path < layout.configs[j].Path
	})
	return layout, nil
}

type bridgeFile struct {
	Path    string       `json:"path"`
	Exports []string     `json:"exports"`
	Imports []ImportEdge `json:"imports"`
}

type bridgeResult struct {
	Warnings []string     `json:"warnings"`
	Files    []bridgeFile `json:"files"`
}

// Analyze returns the version-1 JSON analysis report for sourceInput. It is
// deterministic and never calls a model. Cancellation interrupts CPU-bound
// compiler work instead of waiting for it to finish.
func Analyze(ctx context.Context, sourceInput string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(sourceInput)
	if err != nil {
		return nil, err
	}
	source, err := filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	layout, err := collectSourceFiles(ctx, source)
	if err != nil {
		return nil, err
	}
	raw, err := runTypeScriptAnalysis(ctx, source, layout.names, layout.packages, layout.warnings, string(filepath.Separator), layout.configs)
	if err != nil {
		return nil, err
	}
	var result bridgeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode typescript analysis: %w", err)
	}

	files := make([]SourceFile, 0, len(result.Files))
	for _, analyzed := range result.Files {
		meta := layout.meta[analyzed.Path]
		exports := analyzed.Exports
		if exports == nil {
			exports = []string{}
		}
		imports := analyzed.Imports
		if imports == nil {
			imports = []ImportEdge{}
		}
		files = append(files, SourceFile{
			Path:    analyzed.Path,
			SHA256:  meta.sha256,
			Lines:   meta.lines,
			Test:    meta.test,
			Exports: exports,
			Imports: imports,
		})
	}

	graph := make(map[string][]string, len(files))
	for _, file := range files {
		var targets []string
		for _, edge := range file.Imports {
			if edge.Target != "" {
				targets = append(targets, edge.Target)
			}
		}
		graph[file.Path] = targets
	}
	cycles := [][]string{}
	for _, group := range components(layout.names, graph) {
		if len(group) > 1 || containsString(graph[group[0]], group[0]) {
			cycles = append(cycles, group)
		}
	}
	warnings := result.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	configs := layout.configs
	if configs == nil {
		configs = []AnalysisConfig{}
	}
	analysis := Analysis{
		Version:  1,
		Source:   source,
		Files:    files,
		Cycles:   cycles,
		Warnings: warnings,
		Configs:  configs,
	}
	encoded, err := json.Marshal(analysis)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}
