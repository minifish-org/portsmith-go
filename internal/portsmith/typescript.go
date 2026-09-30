// typescript.go hosts the embedded TypeScript 5.9.3 compiler inside Goja. The
// compiler bundle is a frozen third-party asset (typescript.txt); only a small
// AST/module-resolution bridge is written by this project. The bridge receives
// a read-only host explicitly because the bundle's `ts.sys` getter is bound to
// Node's filesystem and cannot be reassigned.
package portsmith

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/dop251/goja"
)

//go:embed typescript.txt
var typeScriptBundle string

// typeScriptBridge performs the AST traversal and module resolution described
// by src/analyze.ts. It only uses the public TypeScript compiler API and an
// explicit host supplied by Go; it never touches ts.sys.
const typeScriptBridge = `(function () {
  "use strict";
  var ts = globalThis.ts;

  function stripBom(text) {
    return text && text.charCodeAt(0) === 0xFEFF ? text.slice(1) : text;
  }

  var host = {
    useCaseSensitiveFileNames: __hostUseCaseSensitive(),
    getCurrentDirectory: function () { return __hostSource(); },
    fileExists: function (name) { return __hostFileExists(name); },
    readFile: function (name) {
      var text = __hostReadFile(name);
      return text === undefined || text === null ? undefined : stripBom(text);
    },
    directoryExists: function (name) { return __hostDirectoryExists(name); },
    getDirectories: function (name) { return __hostGetDirectories(name); },
    realpath: function (name) { return __hostRealpath(name); },
    readDirectory: function (p, extensions, exclude, include, depth) {
      return __hostReadDirectory(p, extensions || [], exclude || [], include || [], depth || 0);
    },
    trace: function () {}
  };

  globalThis.__portsmithAnalyze = function (params) {
    var source = params.source;
    var sep = params.sep;
    var names = params.names || [];
    var packageNames = params.packages || [];
    var warnings = (params.warnings || []).slice();

    var known = {};
    for (var i = 0; i < names.length; i++) known[__hostJoin(source, names[i])] = true;

    var configCache = {};

    function relativeToSource(name) { return __hostRelative(source, name); }

    function hasNonNodeModulesTarget(targets) {
      for (var t = 0; t < targets.length; t++) {
        var parts = String(targets[t]).split(/[\\/]/);
        var found = false;
        for (var q = 0; q < parts.length; q++) if (parts[q] === "node_modules") found = true;
        if (!found) return true;
      }
      return false;
    }

    function optionsFor(file) {
      var dir = __hostDirname(file);
      var config;
      while (dir === source || dir.indexOf(source + sep) === 0) {
        var candidate = __hostJoin(dir, "tsconfig.json");
        if (__hostFileExists(candidate)) { config = candidate; break; }
        var parent = __hostDirname(dir);
        if (parent === dir) break;
        dir = parent;
      }
      var key = config || "default";
      if (Object.prototype.hasOwnProperty.call(configCache, key)) return configCache[key];
      var options = {
        moduleResolution: ts.ModuleResolutionKind.NodeNext,
        module: ts.ModuleKind.NodeNext,
        allowJs: true
      };
      if (config) {
        var read = ts.readConfigFile(config, host.readFile);
        if (read.error) {
          warnings.push("Configuration parse failed: " + relativeToSource(config));
        } else {
          var parsed = ts.parseJsonConfigFileContent(read.config, host, __hostDirname(config));
          options = Object.assign({}, options, parsed.options);
          for (var j = 0; j < parsed.errors.length; j++) {
            var diag = parsed.errors[j];
            if (diag.code !== 18003) {
              warnings.push(relativeToSource(config) + ": " +
                ts.flattenDiagnosticMessageText(diag.messageText, " "));
            }
          }
        }
      }
      configCache[key] = options;
      return options;
    }

    var grouped = [];
    for (var n = 0; n < names.length; n++) {
      var name = names[n];
      var file = __hostJoin(source, name);
      var text = __hostReadSource(file);
      var ast = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true);
      var options = optionsFor(file);
      var imports = [];
      var exports = [];

      for (var s = 0; s < ast.statements.length; s++) {
        var statement = ast.statements[s];
        if (ts.isExportDeclaration(statement) && statement.exportClause &&
            ts.isNamedExports(statement.exportClause)) {
          for (var e = 0; e < statement.exportClause.elements.length; e++)
            exports.push(statement.exportClause.elements[e].name.text);
        } else if (ts.isExportAssignment(statement)) {
          exports.push("default");
        } else if (ts.canHaveModifiers(statement)) {
          var modifiers = ts.getModifiers(statement);
          var exported = false;
          if (modifiers) for (var m = 0; m < modifiers.length; m++)
            if (modifiers[m].kind === ts.SyntaxKind.ExportKeyword) exported = true;
          if (exported) {
            if (ts.isVariableStatement(statement)) {
              for (var d = 0; d < statement.declarationList.declarations.length; d++)
                exports.push(statement.declarationList.declarations[d].name.getText(ast));
            } else if ("name" in statement && statement.name) {
              exports.push(statement.name.getText(ast));
            }
          }
        }
      }

      function record(expr, typeOnly) {
        var line = ast.getLineAndCharacterOfPosition(expr.getStart(ast)).line + 1;
        if (!ts.isStringLiteralLike(expr)) {
          imports.push({ specifier: expr.getText(ast), kind: "computed", typeOnly: !!typeOnly, line: line });
          return;
        }
        var specifier = expr.text;
        var resolved = ts.resolveModuleName(specifier, file, options, host).resolvedModule;
        var target = resolved && __hostResolve(resolved.resolvedFileName);
        if (target && known[target]) {
          imports.push({ specifier: specifier, kind: "internal", target: relativeToSource(target), typeOnly: !!typeOnly, line: line });
          return;
        }
        var own = false;
        for (var p = 0; p < packageNames.length; p++) {
          var pkg = packageNames[p];
          if (specifier === pkg || specifier.indexOf(pkg + "/") === 0) { own = true; break; }
        }
        var alias = false;
        var paths = options.paths || {};
        var keys = Object.keys(paths);
        for (var k = 0; k < keys.length; k++) {
          var pattern = keys[k];
          if (pattern === "*") continue;
          var targets = paths[pattern] || [];
          if (!hasNonNodeModulesTarget(targets)) continue;
          var matches;
          if (pattern.indexOf("*") >= 0) {
            var halves = pattern.split("*");
            matches = specifier.indexOf(halves[0]) === 0 &&
              specifier.slice(specifier.length - halves[1].length) === halves[1];
          } else {
            matches = pattern === specifier;
          }
          if (matches) { alias = true; break; }
        }
        var isUnresolved = specifier.charAt(0) === "." || __hostIsAbsolute(specifier) || own || alias;
        imports.push({ specifier: specifier, kind: isUnresolved ? "unresolved" : "external", typeOnly: !!typeOnly, line: line });
      }

      function visit(node) {
        if (ts.isImportDeclaration(node)) {
          record(node.moduleSpecifier, node.importClause ? node.importClause.isTypeOnly : false);
        } else if (ts.isExportDeclaration(node) && node.moduleSpecifier) {
          record(node.moduleSpecifier, node.isTypeOnly);
        } else if (ts.isImportEqualsDeclaration(node) &&
                   ts.isExternalModuleReference(node.moduleReference) &&
                   node.moduleReference.expression) {
          record(node.moduleReference.expression, node.isTypeOnly);
        } else if (ts.isCallExpression(node) &&
                   (node.expression.kind === ts.SyntaxKind.ImportKeyword ||
                    (ts.isIdentifier(node.expression) && node.expression.text === "require")) &&
                   node.arguments[0]) {
          record(node.arguments[0], false);
        } else if (ts.isImportTypeNode(node) && ts.isLiteralTypeNode(node.argument)) {
          record(node.argument.literal, true);
        }
        ts.forEachChild(node, visit);
      }
      visit(ast);

      var seenExports = {};
      var uniqueExports = [];
      for (e = 0; e < exports.length; e++) {
        if (!seenExports[exports[e]]) { seenExports[exports[e]] = true; uniqueExports.push(exports[e]); }
      }
      grouped.push({ path: name, exports: uniqueExports, imports: imports });
    }

    var seenWarnings = {};
    var uniqueWarnings = [];
    for (i = 0; i < warnings.length; i++) {
      if (!seenWarnings[warnings[i]]) { seenWarnings[warnings[i]] = true; uniqueWarnings.push(warnings[i]); }
    }
    return JSON.stringify({ warnings: uniqueWarnings, files: grouped });
  };
})();
`

var (
	typeScriptProgramOnce sync.Once
	typeScriptProgram     *goja.Program
	typeScriptProgramErr  error
)

// compiledTypeScript compiles the bundle once so repeated analyses only pay the
// evaluation cost, not the parse cost.
func compiledTypeScript() (*goja.Program, error) {
	typeScriptProgramOnce.Do(func() {
		typeScriptProgram, typeScriptProgramErr = goja.Compile("typescript.js", typeScriptBundle, false)
	})
	return typeScriptProgram, typeScriptProgramErr
}

// typeScriptHost implements the read-only TypeScript compiler host. Every
// callback observes the real filesystem without caching and without following
// the analysis walk's hidden-tree policy: module resolution must be able to see
// whatever the compiler is asked about.
type typeScriptHost struct {
	vm     *goja.Runtime
	source string
}

func (h *typeScriptHost) register() {
	h.vm.Set("__hostSource", func(goja.FunctionCall) goja.Value { return h.vm.ToValue(h.source) })
	h.vm.Set("__hostUseCaseSensitive", func(goja.FunctionCall) goja.Value {
		return h.vm.ToValue(runtime.GOOS != "windows" && runtime.GOOS != "darwin")
	})
	h.vm.Set("__hostReadFile", func(call goja.FunctionCall) goja.Value {
		data, err := os.ReadFile(call.Argument(0).String())
		if err != nil {
			return goja.Undefined()
		}
		if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
			data = data[3:]
		}
		return h.vm.ToValue(string(data))
	})
	h.vm.Set("__hostReadSource", func(call goja.FunctionCall) goja.Value {
		data, err := os.ReadFile(call.Argument(0).String())
		if err != nil {
			return h.vm.ToValue("")
		}
		return h.vm.ToValue(string(data))
	})
	h.vm.Set("__hostFileExists", func(call goja.FunctionCall) goja.Value {
		info, err := os.Stat(call.Argument(0).String())
		return h.vm.ToValue(err == nil && !info.IsDir())
	})
	h.vm.Set("__hostDirectoryExists", func(call goja.FunctionCall) goja.Value {
		info, err := os.Stat(call.Argument(0).String())
		return h.vm.ToValue(err == nil && info.IsDir())
	})
	h.vm.Set("__hostRealpath", func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		resolved, err := filepath.EvalSymlinks(name)
		if err != nil {
			return h.vm.ToValue(name)
		}
		return h.vm.ToValue(resolved)
	})
	h.vm.Set("__hostJoin", func(call goja.FunctionCall) goja.Value {
		parts := make([]string, 0, len(call.Arguments))
		for _, arg := range call.Arguments {
			parts = append(parts, arg.String())
		}
		return h.vm.ToValue(filepath.Join(parts...))
	})
	h.vm.Set("__hostDirname", func(call goja.FunctionCall) goja.Value {
		return h.vm.ToValue(filepath.Dir(call.Argument(0).String()))
	})
	h.vm.Set("__hostRelative", func(call goja.FunctionCall) goja.Value {
		rel, err := filepath.Rel(call.Argument(0).String(), call.Argument(1).String())
		if err != nil {
			return h.vm.ToValue(call.Argument(1).String())
		}
		return h.vm.ToValue(filepath.ToSlash(rel))
	})
	h.vm.Set("__hostResolve", func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		abs, err := filepath.Abs(name)
		if err != nil {
			return h.vm.ToValue(filepath.Clean(name))
		}
		return h.vm.ToValue(filepath.Clean(abs))
	})
	h.vm.Set("__hostIsAbsolute", func(call goja.FunctionCall) goja.Value {
		return h.vm.ToValue(filepath.IsAbs(call.Argument(0).String()))
	})
	h.vm.Set("__hostGetDirectories", func(call goja.FunctionCall) goja.Value {
		root := call.Argument(0).String()
		entries, err := os.ReadDir(root)
		if err != nil {
			return h.vm.ToValue([]string{})
		}
		var dirs []string
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, filepath.Join(root, entry.Name()))
			}
		}
		return h.vm.ToValue(dirs)
	})
	h.vm.Set("__hostReadDirectory", func(call goja.FunctionCall) goja.Value {
		root := call.Argument(0).String()
		extensions := toStringSlice(call.Argument(1))
		var files []string
		_ = filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				if current != root {
					name := entry.Name()
					if name == "node_modules" || strings.HasPrefix(name, ".") {
						return filepath.SkipDir
					}
				}
				return nil
			}
			for _, extension := range extensions {
				if strings.HasSuffix(current, extension) {
					files = append(files, current)
					break
				}
			}
			return nil
		})
		return h.vm.ToValue(files)
	})
}

func toStringSlice(value goja.Value) []string {
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return nil
	}
	exported := value.Export()
	switch items := exported.(type) {
	case []string:
		return items
	case []interface{}:
		result := make([]string, 0, len(items))
		for _, item := range items {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

// runTypeScriptAnalysis evaluates the bridge in a fresh runtime and returns its
// JSON result. CPU-bound compiler work is interrupted through runtime.Interrupt
// when the context is cancelled; the runtime is discarded on return.
func runTypeScriptAnalysis(ctx context.Context, source string, names, packages, warnings []string, sep string, configs []AnalysisConfig) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	program, err := compiledTypeScript()
	if err != nil {
		return nil, fmt.Errorf("compile typescript: %w", err)
	}
	vm := goja.New()
	host := &typeScriptHost{vm: vm, source: source}
	host.register()

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
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("load typescript: %w", err)
	}
	if _, err := vm.RunString(typeScriptBridge); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("load typescript bridge: %w", err)
	}
	callable, ok := goja.AssertFunction(vm.Get("__portsmithAnalyze"))
	if !ok {
		return nil, fmt.Errorf("typescript bridge did not expose an analyzer")
	}
	params := map[string]any{
		"source":   source,
		"sep":      sep,
		"names":    names,
		"packages": packages,
		"warnings": warnings,
		"configs":  configs,
	}
	value, err := callable(goja.Undefined(), vm.ToValue(params))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("typescript analysis: %w", err)
	}
	if value == nil || goja.IsUndefined(value) {
		return nil, fmt.Errorf("typescript analysis returned no result")
	}
	return json.RawMessage(value.String()), nil
}
