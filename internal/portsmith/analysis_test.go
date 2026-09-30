package portsmith

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// analysisFixture mirrors the pinned TypeScript compiler oracle fixture used by
// the independent judge and by the upstream workflow tests. The golden report
// below is the pinned expected.json; the source path is removed before
// comparison because it is always canonicalized to the temporary directory.
const analysisGolden = `{
  "version": 1,
  "source": "<fixture>",
  "files": [
    {
      "path": "cycle/a.ts",
      "sha256": "78fd763fb31179d16c2e6913acb7cb89caa4d3a92611962fcf165dd6827e9240",
      "lines": 2,
      "test": false,
      "exports": ["a"],
      "imports": [
        {"specifier": "./b.js", "kind": "internal", "target": "cycle/b.ts", "typeOnly": false, "line": 1}
      ]
    },
    {
      "path": "cycle/b.ts",
      "sha256": "460037913078be30436fc43aa2b87a44c8dc8a865a02fab91b9c30fabc21d090",
      "lines": 2,
      "test": false,
      "exports": ["b"],
      "imports": [
        {"specifier": "./a.js", "kind": "internal", "target": "cycle/a.ts", "typeOnly": false, "line": 1}
      ]
    },
    {
      "path": "legacy/require.cts",
      "sha256": "f76fae63a81fdb0ce09b0c5f3c2a83aa180ca517d41e2dc887d55715ef45f590",
      "lines": 2,
      "test": false,
      "exports": ["default"],
      "imports": [
        {"specifier": "../src/types.js", "kind": "internal", "target": "src/types.ts", "typeOnly": false, "line": 1}
      ]
    },
    {
      "path": "src/index.ts",
      "sha256": "89a31f76c55a0cc9c6097a567be9f1afa2f764da4880ee5348364430c1f2dd86",
      "lines": 11,
      "test": false,
      "exports": ["answer", "Shape", "Missing", "result"],
      "imports": [
        {"specifier": "@src/value", "kind": "internal", "target": "src/value.ts", "typeOnly": false, "line": 1},
        {"specifier": "./value.js", "kind": "internal", "target": "src/value.ts", "typeOnly": false, "line": 2},
        {"specifier": "./types.js", "kind": "internal", "target": "src/types.ts", "typeOnly": true, "line": 3},
        {"specifier": "./absent.js", "kind": "unresolved", "typeOnly": true, "line": 4},
        {"specifier": "name", "kind": "computed", "typeOnly": false, "line": 6},
        {"specifier": "node:fs", "kind": "external", "typeOnly": false, "line": 7},
        {"specifier": "third-party", "kind": "external", "typeOnly": false, "line": 8},
        {"specifier": "fixture-workspace/missing", "kind": "unresolved", "typeOnly": false, "line": 9},
        {"specifier": "@src/missing", "kind": "unresolved", "typeOnly": false, "line": 10}
      ]
    },
    {
      "path": "src/types.ts",
      "sha256": "def06374fa7bf989e07f85a09bf15296ddd82f3a0319e32380f4588ffbb5f18d",
      "lines": 2,
      "test": false,
      "exports": ["Shape", "Numberish"],
      "imports": []
    },
    {
      "path": "src/value.ts",
      "sha256": "c2c5fa09c87f73e9cbe9ceed2802d406f0c0815dcdf11973c02009a1d46dde55",
      "lines": 3,
      "test": false,
      "exports": ["value", "default"],
      "imports": [
        {"specifier": "./types.js", "kind": "internal", "target": "src/types.ts", "typeOnly": true, "line": 1}
      ]
    },
    {
      "path": "tests/value.test.ts",
      "sha256": "f6f1e0ec4ec8a8e97cbea3d2171a1894f649b561f5ab1da049cd543be6a939a2",
      "lines": 2,
      "test": true,
      "exports": ["check"],
      "imports": [
        {"specifier": "../src/value.js", "kind": "internal", "target": "src/value.ts", "typeOnly": false, "line": 1}
      ]
    },
    {
      "path": "types.d.ts",
      "sha256": "c990ef39b8ffc9062ea2f9848a98d9a34e236388ec364d3e706fa6d2337ffeb0",
      "lines": 1,
      "test": false,
      "exports": [],
      "imports": []
    },
    {
      "path": "view/component.tsx",
      "sha256": "12074969dcb2fac44a4f5f15c423c2b97dfb36d215fd5a91bc08344184d1a8c5",
      "lines": 2,
      "test": false,
      "exports": ["Component"],
      "imports": [
        {"specifier": "../src/types.js", "kind": "internal", "target": "src/types.ts", "typeOnly": true, "line": 1}
      ]
    }
  ],
  "cycles": [["cycle/a.ts", "cycle/b.ts"]],
  "warnings": [],
  "configs": [
    {"path": "package.json", "sha256": "5ea38992f5001e0aa4b23a9b8ad164f629f8545cea29f92f9b260b204674d603"},
    {"path": "tsconfig.json", "sha256": "0973306982097480d9496bfe716b45eaf208b4e37d11b6acdaf45206a0637f43"}
  ]
}`

func writeAnalysisFixture(t *testing.T, root string) {
	t.Helper()
	testPut(t, root, "cycle/a.ts", "import { b } from \"./b.js\";\nexport const a = () => b;\n")
	testPut(t, root, "cycle/b.ts", "import { a } from \"./a.js\";\nexport const b = () => a;\n")
	testPut(t, root, "legacy/require.cts", "import x = require(\"../src/types.js\");\nexport = x;\n")
	testPut(t, root, "package.json", "{\"name\":\"fixture-workspace\",\"type\":\"module\"}\n")
	testPut(t, root, "tsconfig.json", "{\"compilerOptions\":{\"module\":\"NodeNext\",\"moduleResolution\":\"NodeNext\",\"baseUrl\":\".\",\"paths\":{\"@src/*\":[\"src/*\"],\"*\": [\"fallback/*\"]}}}\n")
	testPut(t, root, "src/index.ts", strings.Join([]string{
		"import { value } from \"@src/value\";",
		"export { value as answer } from \"./value.js\";",
		"export type { Shape } from \"./types.js\";",
		"export type Missing = import(\"./absent.js\").Missing;",
		"const name = \"dynamic\";",
		"import(name);",
		"require(\"node:fs\");",
		"import(\"third-party\");",
		"import(\"fixture-workspace/missing\");",
		"import(\"@src/missing\");",
		"export const result = value;",
		"",
	}, "\n"))
	testPut(t, root, "src/types.ts", "export interface Shape { value: number }\nexport type Numberish = number | null;\n")
	testPut(t, root, "src/value.ts", "import type { Shape } from \"./types.js\";\nexport const value: Shape = { value: 42 };\nexport default value;\n")
	testPut(t, root, "tests/value.test.ts", "import { value } from \"../src/value.js\";\nexport const check = () => value;\n")
	testPut(t, root, "types.d.ts", "declare module \"third-party\" { export const x: number; }\n")
	testPut(t, root, "view/component.tsx", "import type {Shape} from \"../src/types.js\";\nexport const Component = (p:Shape) => <span>{p.value}</span>;\n")
	testPut(t, root, "ignored.txt", "not source\n")
	testPut(t, root, "node_modules/ignored/index.ts", "export const ignored = true;\n")
	testPut(t, root, "dist/ignored.ts", "export const ignored = true;\n")
}

func analyzeFixture(t *testing.T, root string) Analysis {
	t.Helper()
	raw, err := Analyze(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var analysis Analysis
	if err := json.Unmarshal(raw, &analysis); err != nil {
		t.Fatal(err)
	}
	return analysis
}

func TestComponentsDependencyFirst(t *testing.T) {
	groups := components(
		[]string{"a", "b", "c", "d"},
		map[string][]string{"a": {"b"}, "b": {"a"}, "c": {"a"}, "d": {}},
	)
	if len(groups) != 3 {
		t.Fatalf("unexpected groups: %v", groups)
	}
	// a and b form a cycle; c depends on the cycle; d is independent.
	if !reflect.DeepEqual(groups[0], []string{"a", "b"}) {
		t.Fatalf("first group: %v", groups[0])
	}
	if !reflect.DeepEqual(groups[1], []string{"c"}) || !reflect.DeepEqual(groups[2], []string{"d"}) {
		t.Fatalf("order not dependency-first: %v", groups)
	}
}

func TestAnalyzePinnedCompilerOracleParity(t *testing.T) {
	root := t.TempDir()
	writeAnalysisFixture(t, root)
	raw, err := Analyze(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(analysisGolden), &want); err != nil {
		t.Fatal(err)
	}
	delete(got, "source")
	delete(want, "source")
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("analysis diverges from the pinned compiler oracle\ngot: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestAnalyzeResolvesExportsTypesAndImports(t *testing.T) {
	root := t.TempDir()
	testPut(t, root, "tsconfig.json", `{"compilerOptions":{"baseUrl":".","paths":{"@lib/*":["lib/*"]}}}`)
	testPut(t, root, "package.json", `{"name":"@local/project"}`)
	testPut(t, root, "main.ts", strings.Join([]string{
		"import {a} from '@lib/a.js';",
		"export {b} from './lib/b.js';",
		"// import 'fake'",
		"const s=\"import x from 'fake2'\";",
		"import('@local/project/missing');",
		"import('./missing.js');",
		"import(variable);",
		"import type {Stats} from 'node:fs';",
		"type T=import('./lib/b.js').B;",
		"",
	}, "\n"))
	testPut(t, root, "lib/a.ts", "import './b.js'; export const a=1;\n")
	testPut(t, root, "lib/b.ts", "import './a.js'; export const b=2; export type B=number;\n")
	analysis := analyzeFixture(t, root)
	var main *SourceFile
	for i := range analysis.Files {
		if analysis.Files[i].Path == "main.ts" {
			main = &analysis.Files[i]
		}
	}
	if main == nil {
		t.Fatal("main.ts missing")
	}
	counts := map[string]int{}
	for _, edge := range main.Imports {
		counts[edge.Kind]++
	}
	if counts["internal"] != 3 || counts["unresolved"] != 2 || counts["computed"] != 1 || counts["external"] != 1 {
		t.Fatalf("unexpected edge classification: %v (%v)", counts, main.Imports)
	}
	for _, edge := range main.Imports {
		if strings.HasPrefix(edge.Specifier, "fake") {
			t.Fatalf("comment/string literal was treated as an import: %v", edge)
		}
	}
	if !reflect.DeepEqual(analysis.Cycles, [][]string{{"lib/a.ts", "lib/b.ts"}}) {
		t.Fatalf("unexpected cycles: %v", analysis.Cycles)
	}
}

func TestAnalyzeCatchAllPathsAndNodeModulesAliases(t *testing.T) {
	root := t.TempDir()
	testPut(t, root, "tsconfig.json", `{"compilerOptions":{"baseUrl":".","paths":{"*":["./*"],"typebox":["./node_modules/typebox"],"@local/*":["./src/*"]}}}`)
	testPut(t, root, "main.ts", "import 'node:fs'; import 'vitest'; import 'typebox'; import '@local/missing'; import 'helper';\n")
	testPut(t, root, "helper.ts", "export const value=1;\n")
	analysis := analyzeFixture(t, root)
	var main *SourceFile
	for i := range analysis.Files {
		if analysis.Files[i].Path == "main.ts" {
			main = &analysis.Files[i]
		}
	}
	var external, unresolved, helperTarget []string
	for _, edge := range main.Imports {
		switch edge.Kind {
		case "external":
			external = append(external, edge.Specifier)
		case "unresolved":
			unresolved = append(unresolved, edge.Specifier)
		}
		if edge.Specifier == "helper" {
			helperTarget = append(helperTarget, edge.Target)
		}
	}
	if !reflect.DeepEqual(external, []string{"node:fs", "vitest", "typebox"}) {
		t.Fatalf("catch-all fallback or node_modules alias treated as internal: %v", external)
	}
	if !reflect.DeepEqual(unresolved, []string{"@local/missing"}) {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if !reflect.DeepEqual(helperTarget, []string{"helper.ts"}) {
		t.Fatalf("catch-all fallback did not resolve an internal file: %v", helperTarget)
	}
}

func TestAnalyzeSkipsHiddenAndDependencyTrees(t *testing.T) {
	root := t.TempDir()
	testPut(t, root, "keep.ts", "export const keep = 1;\n")
	for _, name := range []string{
		"node_modules/pkg/index.ts",
		"dist/index.ts",
		"build/index.ts",
		"coverage/index.ts",
		"vendor/index.ts",
		".hidden/index.ts",
	} {
		testPut(t, root, name, "export const ignored = true;\n")
	}
	analysis := analyzeFixture(t, root)
	var paths []string
	for _, file := range analysis.Files {
		paths = append(paths, file.Path)
	}
	if !reflect.DeepEqual(paths, []string{"keep.ts"}) {
		t.Fatalf("hidden/dependency trees leaked into analysis: %v", paths)
	}
}

func TestAnalyzeSkipsSymlinksWithWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably permitted on Windows")
	}
	root := t.TempDir()
	testPut(t, root, "real.ts", "export const value = 1;\n")
	if err := os.Symlink(filepath.Join(root, "real.ts"), filepath.Join(root, "link.ts")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	analysis := analyzeFixture(t, root)
	if len(analysis.Files) != 1 || analysis.Files[0].Path != "real.ts" {
		t.Fatalf("symlink was not skipped: %v", analysis.Files)
	}
	if len(analysis.Warnings) != 1 || analysis.Warnings[0] != "Skipped symbolic link: link.ts" {
		t.Fatalf("missing symlink warning: %v", analysis.Warnings)
	}
}

func TestAnalyzeDoesNotCacheAcrossEdits(t *testing.T) {
	root := t.TempDir()
	testPut(t, root, "value.ts", "export const value = 1;\n")
	first := analyzeFixture(t, root)
	testPut(t, root, "value.ts", "export const value = 9;\n")
	second := analyzeFixture(t, root)
	if reflect.DeepEqual(first, second) {
		t.Fatal("analysis ignored an edited source file")
	}
	if first.Files[0].SHA256 == second.Files[0].SHA256 {
		t.Fatal("hash was not recomputed after an edit")
	}
}

func TestAnalyzeHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	testPut(t, root, "value.ts", "export const value = 1;\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Analyze(ctx, root); err == nil {
		t.Fatal("analyze ignored a cancelled context")
	}
}

func TestCreatePlanDraftShape(t *testing.T) {
	root := t.TempDir()
	writeAnalysisFixture(t, root)
	raw, err := Analyze(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	analysisPath := filepath.Join(root, "analysis.json")
	if err := os.WriteFile(analysisPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(root, "plan")
	planRaw, err := CreatePlan(analysisPath, planPath, "reviewed-revision")
	if err != nil {
		t.Fatal(err)
	}
	var plan Plan
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Version != 1 || plan.Revision != "reviewed-revision" {
		t.Fatalf("unexpected plan header: %+v", plan)
	}
	if plan.AnalysisSHA256 != Hash(raw) {
		t.Fatal("plan not bound to exact analysis bytes")
	}
	var order []string
	for _, unit := range plan.Units {
		order = append(order, unit.ID)
	}
	if !reflect.DeepEqual(order, []string{"cycle", "src", "legacy", "view"}) {
		t.Fatalf("units not dependency-ordered: %v", order)
	}
	for _, unit := range plan.Units {
		if unit.ID == "src" {
			if !reflect.DeepEqual(unit.DependsOn, []string{}) || !reflect.DeepEqual(unit.References, []string{"tests/value.test.ts"}) {
				t.Fatalf("unexpected src unit: %+v", unit)
			}
			if len(unit.Notes) != 6 {
				t.Fatalf("unexpected src notes: %v", unit.Notes)
			}
		}
		if unit.ID == "legacy" || unit.ID == "view" {
			if !reflect.DeepEqual(unit.DependsOn, []string{"src"}) {
				t.Fatalf("unexpected dependencies for %s: %v", unit.ID, unit.DependsOn)
			}
		}
	}
	for _, name := range []string{"analysis.json", "plan.json", "RULEBOOK.md", "README.md"} {
		if _, err := os.Stat(filepath.Join(planPath, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	rules, err := os.ReadFile(filepath.Join(planPath, "RULEBOOK.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(rules) != DEFAULT_RULES {
		t.Fatal("RULEBOOK text diverges from the migrated plan rules")
	}
}

func TestCreatePlanDetectsPackageCycles(t *testing.T) {
	root := t.TempDir()
	testPut(t, root, "a/one.ts", "import '../b/one.js';\n")
	testPut(t, root, "b/one.ts", "export const a=1;\n")
	testPut(t, root, "b/two.ts", "import '../a/two.js';\n")
	testPut(t, root, "a/two.ts", "export const b=2;\n")
	raw, err := Analyze(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	analysisPath := filepath.Join(root, "analysis.json")
	if err := os.WriteFile(analysisPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	planRaw, err := CreatePlan(analysisPath, filepath.Join(root, "plan"), "test")
	if err != nil {
		t.Fatal(err)
	}
	var plan Plan
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.PackageCycles) != 1 {
		t.Fatalf("expected one Go package cycle: %v", plan.PackageCycles)
	}
}

func TestCreatePlanRejectsEmptyAnalysis(t *testing.T) {
	root := t.TempDir()
	analysisPath := filepath.Join(root, "analysis.json")
	if err := os.WriteFile(analysisPath, []byte(`{"version":1,"source":"/x","files":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreatePlan(analysisPath, filepath.Join(root, "plan"), "test"); err == nil {
		t.Fatal("empty analysis accepted")
	}
}

func TestSelectUnitRequiresAcceptanceAndDetectsDrift(t *testing.T) {
	root := t.TempDir()
	testPut(t, root, "main.ts", "export const answer=42;\n")
	testPut(t, root, "tsconfig.json", "{}\n")
	raw, err := Analyze(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	analysisPath := filepath.Join(root, "analysis.json")
	if err := os.WriteFile(analysisPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(root, "plan")
	if _, err := CreatePlan(analysisPath, planPath, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := selectUnit(planPath, "root", root); err == nil || !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("expected acceptance error, got %v", err)
	}
	plan, err := loadPlan(planPath)
	if err != nil {
		t.Fatal(err)
	}
	plan.Units[0].Acceptance = []string{"answer=42"}
	if err := AtomicJSON(filepath.Join(planPath, "plan.json"), plan); err != nil {
		t.Fatal(err)
	}
	if _, err := selectUnit(planPath, "root", root); err != nil {
		t.Fatalf("ready unit rejected: %v", err)
	}
	testPut(t, root, "main.ts", "export const answer=43;\n")
	if _, err := selectUnit(planPath, "root", root); err == nil || !strings.Contains(err.Error(), "Source") {
		t.Fatalf("expected source drift error, got %v", err)
	}
	testPut(t, root, "main.ts", "export const answer=42;\n")
	testPut(t, root, "tsconfig.json", "{\"compilerOptions\":{}}\n")
	if _, err := selectUnit(planPath, "root", root); err == nil || !strings.Contains(err.Error(), "Configuration") {
		t.Fatalf("expected configuration drift error, got %v", err)
	}
}
