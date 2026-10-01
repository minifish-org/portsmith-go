// modules.go ports src/modules.ts: version-2 module workflows with batches,
// cumulative independent gates, same-module writable accumulation, prior
// module read-only seeds, immutable static assets, separate additive journals
// and whole-module commits.
package portsmith

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// moduleDef is one planned module (`Module` in src/modules.ts).
type moduleDef struct {
	ID        string   `json:"id"`
	DependsOn []string `json:"dependsOn"`
	Batches   []string `json:"batches"`
}

// batchDef is one planned batch (`Batch` in src/modules.ts).
type batchDef struct {
	ID         string   `json:"id"`
	Module     string   `json:"module"`
	DependsOn  []string `json:"dependsOn"`
	Sources    []string `json:"sources"`
	References []string `json:"references"`
	Outputs    []string `json:"outputs"`
	Behaviors  []string `json:"behaviors"`
	Acceptance []string `json:"acceptance"`
}

// modulePlan is the version-2 `plan.json` document.
type modulePlan struct {
	Version        int         `json:"version"`
	Source         string      `json:"source"`
	Revision       string      `json:"revision"`
	AnalysisSHA256 string      `json:"analysisSha256"`
	Modules        []moduleDef `json:"modules"`
	Batches        []batchDef  `json:"batches"`
}

// stepAsset is one immutable static asset.
type stepAsset struct {
	Source string `json:"source"`
	Target string `json:"target"`
	SHA256 string `json:"sha256"`
}

// stepDef is one executable step (`Step` in src/modules.ts).
type stepDef struct {
	ID       string      `json:"id"`
	Sources  []string    `json:"sources"`
	Goal     string      `json:"goal"`
	Contract string      `json:"contract"`
	Judge    string      `json:"judge"`
	Outputs  []string    `json:"outputs"`
	Tests    []string    `json:"tests"`
	Race     bool        `json:"race,omitempty"`
	Assets   []stepAsset `json:"assets,omitempty"`
}

// batchConfig is one batch preparation entry.
type batchConfig struct {
	Status string    `json:"status"`
	Reason string    `json:"reason,omitempty"`
	Steps  []stepDef `json:"steps"`
}

// baselineConfig freezes existing product files at a reviewed commit.
type baselineConfig struct {
	Commit string        `json:"commit"`
	Files  []NamedDigest `json:"files"`
}

// moduleWorkflow is the version-2 `workflow.json` document.
type moduleWorkflow struct {
	Version     int                    `json:"version"`
	Project     string                 `json:"project"`
	Runs        string                 `json:"runs"`
	Bootstrap   []string               `json:"bootstrap"`
	StartPolicy string                 `json:"startPolicy,omitempty"`
	Journal     string                 `json:"journal,omitempty"`
	Baseline    *baselineConfig        `json:"baseline,omitempty"`
	Updates     *baselineConfig        `json:"updates,omitempty"`
	Batches     map[string]batchConfig `json:"batches"`
}

// moduleItem is one validated step with its frozen materials.
type moduleItem struct {
	Key      string
	Module   string
	Batch    string
	Spec     stepDef
	Seal     string
	Contract string
	Judge    []File
	Assets   []File
}

// moduleDone is one checkpointed step.
type moduleDone struct {
	Key         string        `json:"key"`
	Seal        string        `json:"seal"`
	Task        string        `json:"task"`
	Fingerprint string        `json:"fingerprint"`
	Files       []NamedDigest `json:"files"`
}

// moduleCommit is one committed module.
type moduleCommit struct {
	ID     string        `json:"id"`
	Commit string        `json:"commit"`
	Files  []NamedDigest `json:"files"`
}

// modulePending is the version-2 transactional integration record.
type modulePending struct {
	Module      string        `json:"module"`
	Base        string        `json:"base"`
	Files       []NamedDigest `json:"files"`
	Message     string        `json:"message"`
	Task        string        `json:"task"`
	Fingerprint string        `json:"fingerprint"`
	Staging     string        `json:"staging"`
	Before      []NamedDigest `json:"before,omitempty"`
}

// moduleState is the version-2 migration journal.
type moduleState struct {
	Version  int            `json:"version"`
	Identity string         `json:"identity"`
	Steps    []moduleDone   `json:"steps"`
	Modules  []moduleCommit `json:"modules"`
	Pending  *modulePending `json:"pending,omitempty"`
	Attempts map[string]int `json:"attempts"`
}

// moduleInspection is the fully validated version-2 execution snapshot.
type moduleInspection struct {
	Root     string
	Plan     modulePlan
	Workflow moduleWorkflow
	Project  string
	Source   string
	Mod      []byte
	Sum      []byte
	Items    []moduleItem
	Identity string
	Baseline []File
	Updates  []File
}

// blockedBatch is one unavailable batch. The module member is supplied for the
// preparation report and omitted for the per-module next-work projection.
type blockedBatch struct {
	Module string `json:"module,omitempty"`
	Batch  string `json:"batch"`
	Reason string `json:"reason"`
}

var moduleOutputPattern = regexp.MustCompile(`\.(go|json|txt|md|yaml|yml|csv)$`)

// outputName validates a module output path against the upstream allowlist.
func outputName(name string) error {
	if _, err := RelativeName(name); err != nil {
		return err
	}
	invalid := !moduleOutputPattern.MatchString(name)
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "portsmith_judge") || strings.HasPrefix(part, "port_oracle") {
			invalid = true
		}
	}
	if name == "go.mod" || name == "go.sum" || name == "LICENSE" || strings.HasPrefix(name, "migration/") {
		invalid = true
	}
	if invalid {
		return fmt.Errorf("Module output is not allowed: %s", name)
	}
	return nil
}

type graphNode struct {
	ID        string
	DependsOn []string
}

// graph validates IDs, references and cycles for modules and batches.
func graph(nodes []graphNode) error {
	index := make(map[string]graphNode, len(nodes))
	for _, node := range nodes {
		if !unitIDPattern.MatchString(node.ID) {
			return errors.New("Duplicate or invalid ID")
		}
		if _, exists := index[node.ID]; exists {
			return errors.New("Duplicate or invalid ID")
		}
		index[node.ID] = node
	}
	done := map[string]bool{}
	visiting := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		node, ok := index[id]
		if !ok {
			return fmt.Errorf("Unknown dependency: %s", id)
		}
		if done[id] {
			return nil
		}
		if visiting[id] {
			return fmt.Errorf("Dependency cycle: %s", id)
		}
		visiting[id] = true
		for _, dep := range node.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		delete(visiting, id)
		done[id] = true
		return nil
	}
	for _, node := range nodes {
		if err := visit(node.ID); err != nil {
			return err
		}
	}
	return nil
}

// inspectModules fully validates a version-2 module workflow before any model
// call, task directory or commit.
func inspectModules(planInput string) (*moduleInspection, error) {
	absolute, err := filepath.Abs(planInput)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	plan, err := readJSON[modulePlan](root, "plan.json")
	if err != nil {
		return nil, err
	}
	workflow, err := readJSON[moduleWorkflow](root, "workflow.json")
	if err != nil {
		return nil, err
	}
	if plan.Version != 2 || workflow.Version != 2 || plan.Modules == nil || plan.Batches == nil ||
		workflow.Batches == nil || workflow.Bootstrap == nil {
		return nil, errors.New("Incomplete module workflow: v2 modules/batches and execution steps are required")
	}
	if workflow.StartPolicy != "" && workflow.StartPolicy != "all-prepared" && workflow.StartPolicy != "available-steps" {
		return nil, errors.New("startPolicy must be all-prepared or available-steps")
	}
	moduleNodes := make([]graphNode, 0, len(plan.Modules))
	for _, module := range plan.Modules {
		moduleNodes = append(moduleNodes, graphNode{ID: module.ID, DependsOn: module.DependsOn})
	}
	batchNodes := make([]graphNode, 0, len(plan.Batches))
	for _, batch := range plan.Batches {
		batchNodes = append(batchNodes, graphNode{ID: batch.ID, DependsOn: batch.DependsOn})
	}
	if err := graph(moduleNodes); err != nil {
		return nil, err
	}
	if err := graph(batchNodes); err != nil {
		return nil, err
	}
	projectInput := workflow.Project
	if !filepath.IsAbs(projectInput) {
		projectInput = filepath.Join(root, projectInput)
	}
	project, err := filepath.EvalSymlinks(projectInput)
	if err != nil {
		return nil, err
	}
	top, err := gitRun(context.Background(), project, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if top != project {
		return nil, errors.New("project must be the target Git root")
	}
	if _, err := RelativeName(workflow.Runs); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(workflow.Runs, ".portsmith/") {
		return nil, errors.New("Module task directories must be under .portsmith")
	}
	for _, name := range workflow.Bootstrap {
		if _, err := RelativeName(name); err != nil {
			return nil, err
		}
		gitSegment := false
		for _, segment := range strings.Split(name, "/") {
			if strings.HasPrefix(segment, ".git") && segment != ".gitignore" {
				gitSegment = true
			}
		}
		if gitSegment || strings.HasPrefix(name, ".env") || strings.HasPrefix(name, ".portsmith") {
			return nil, errors.New("bootstrap must not include credentials or internal state")
		}
	}
	sourceInput := plan.Source
	if !filepath.IsAbs(sourceInput) {
		sourceInput = filepath.Join(project, filepath.FromSlash(plan.Source))
	}
	source, err := filepath.EvalSymlinks(sourceInput)
	if err != nil {
		return nil, err
	}
	analysisRaw, err := readCheckedBytes(root, "analysis.json")
	if err != nil {
		return nil, err
	}
	if Hash(analysisRaw) != plan.AnalysisSHA256 {
		return nil, errors.New("Analysis snapshot changed")
	}
	var analysis Analysis
	if err := json.Unmarshal(analysisRaw, &analysis); err != nil {
		return nil, err
	}
	known := map[string]string{}
	for _, file := range analysis.Files {
		known[file.Path] = file.SHA256
	}
	for _, config := range analysis.Configs {
		data, err := readCheckedBytes(source, config.Path)
		if err != nil {
			return nil, err
		}
		if Hash(data) != config.SHA256 {
			return nil, fmt.Errorf("Source configuration changed: %s", config.Path)
		}
	}
	rules, err := readCheckedBytes(root, "RULEBOOK.md")
	if err != nil {
		return nil, err
	}
	mod, err := readCheckedBytes(project, "go.mod")
	if err != nil {
		return nil, err
	}
	hasSum, err := fileExists(filepath.Join(project, "go.sum"))
	if err != nil {
		return nil, err
	}
	var sum []byte
	if hasSum {
		if sum, err = readCheckedBytes(project, "go.sum"); err != nil {
			return nil, err
		}
	}
	if _, err := CheckedFile(root, "go.mod"); err != nil {
		return nil, err
	}
	batchesByID := make(map[string]batchDef, len(plan.Batches))
	for _, batch := range plan.Batches {
		batchesByID[batch.ID] = batch
	}
	for id := range workflow.Batches {
		if _, ok := batchesByID[id]; !ok {
			return nil, errors.New("Workflow contains a batch not listed in the plan")
		}
	}
	listed := []string{}
	for _, module := range plan.Modules {
		listed = append(listed, module.Batches...)
	}
	listedSet := map[string]bool{}
	for _, id := range listed {
		listedSet[id] = true
	}
	if len(listedSet) != len(plan.Batches) || len(listed) != len(plan.Batches) {
		return nil, errors.New("Batch ownership is incomplete")
	}
	for _, id := range listed {
		if _, ok := batchesByID[id]; !ok {
			return nil, errors.New("Batch ownership is incomplete")
		}
	}

	items := []moduleItem{}
	outputOwners := map[string]string{}
	assetOwners := map[string]bool{}
	judgeOwners := map[string]bool{}
	for _, module := range plan.Modules {
		for _, id := range module.Batches {
			batch := batchesByID[id]
			if batch.Module != module.ID {
				return nil, fmt.Errorf("Invalid batch ownership/dependencies: %s", id)
			}
			for _, dependency := range batch.DependsOn {
				if batchesByID[dependency].Module != module.ID {
					return nil, fmt.Errorf("Invalid batch ownership/dependencies: %s", id)
				}
			}
			cfg, ok := workflow.Batches[id]
			if !ok || (cfg.Status != "ready" && cfg.Status != "partial" && cfg.Status != "planned") || cfg.Steps == nil || (cfg.Status != "ready" && cfg.Reason == "") {
				return nil, fmt.Errorf("Missing batch preparation status: %s", id)
			}
			if (cfg.Status == "planned" && len(cfg.Steps) != 0) || (cfg.Status == "ready" && len(cfg.Steps) == 0) {
				return nil, fmt.Errorf("Batch status does not match its steps: %s", id)
			}
			seenStepIDs := map[string]bool{}
			for _, step := range cfg.Steps {
				if !unitIDPattern.MatchString(step.ID) || seenStepIDs[step.ID] || strings.TrimSpace(step.Goal) == "" ||
					len(step.Sources) == 0 || len(step.Outputs) == 0 || len(step.Tests) == 0 {
					return nil, fmt.Errorf("Invalid step configuration: %s/%s", id, step.ID)
				}
				seenStepIDs[step.ID] = true
				sources := []NamedDigest{}
				availableSources := append(append([]string{}, batch.Sources...), batch.References...)
				for _, name := range step.Sources {
					if !containsString(availableSources, name) {
						return nil, fmt.Errorf("Source is not part of the batch: %s/%s", id, name)
					}
					data, err := readCheckedBytes(source, name)
					if err != nil {
						return nil, err
					}
					if sha, ok := known[name]; !ok || sha != Hash(data) {
						return nil, fmt.Errorf("Source changed after analysis: %s", name)
					}
					sources = append(sources, NamedDigest{Name: name, SHA256: Hash(data)})
				}
				contract, err := readCheckedBytes(root, step.Contract)
				if err != nil {
					return nil, err
				}
				assets := []File{}
				for _, asset := range step.Assets {
					if err := outputName(asset.Target); err != nil {
						return nil, err
					}
					if !containsString(batch.Outputs, asset.Target) || assetOwners[asset.Target] || containsString(step.Outputs, asset.Target) {
						return nil, fmt.Errorf("Static asset target conflicts or is not registered: %s", asset.Target)
					}
					data, err := readCheckedBytes(root, asset.Source)
					if err != nil {
						return nil, err
					}
					if Hash(data) != asset.SHA256 {
						return nil, fmt.Errorf("Static asset changed or exceeds a configured limit: %s", asset.Source)
					}
					assets = append(assets, File{Name: asset.Target, Data: data, SHA256: Hash(data)})
					outputOwners[asset.Target] = module.ID
					assetOwners[asset.Target] = true
				}
				judge, err := snapshotFiles(filepath.Join(root, filepath.FromSlash(step.Judge)), 0, 0, nil)
				if err != nil {
					return nil, err
				}
				var testText strings.Builder
				for _, file := range judge {
					if strings.HasSuffix(file.Name, "_test.go") {
						testText.Write(file.Data)
						testText.WriteByte('\n')
					}
				}
				testsSeen := map[string]bool{}
				for _, name := range step.Tests {
					if testsSeen[name] || !v1JudgeNamePattern.MatchString(name) || !testFunctionPresent(testText.String(), name) {
						return nil, fmt.Errorf("Missing independent tests: %s/%s", id, step.ID)
					}
					testsSeen[name] = true
				}
				hasImpl := false
				hasTest := false
				for _, name := range step.Outputs {
					if strings.HasSuffix(name, "_test.go") {
						hasTest = true
					} else if strings.HasSuffix(name, ".go") {
						hasImpl = true
					}
				}
				if !hasImpl || !hasTest {
					return nil, fmt.Errorf("Step requires implementation and candidate tests: %s/%s", id, step.ID)
				}
				for _, name := range step.Outputs {
					if err := outputName(name); err != nil {
						return nil, err
					}
					if assetOwners[name] {
						return nil, fmt.Errorf("Static assets cannot be declared as writable outputs: %s", name)
					}
					covered := containsString(batch.Outputs, name)
					if !covered && strings.HasSuffix(name, "_test.go") {
						for _, output := range batch.Outputs {
							if path.Dir(output) == path.Dir(name) {
								covered = true
								break
							}
						}
					}
					if !covered {
						return nil, fmt.Errorf("Output is not listed in the plan: %s/%s", id, name)
					}
					if owner, ok := outputOwners[name]; ok && owner != module.ID {
						return nil, fmt.Errorf("Module output conflict: %s", name)
					}
					outputOwners[name] = module.ID
				}
				for _, file := range judge {
					if _, err := RelativeName(file.Name); err != nil {
						return nil, err
					}
					valid := strings.HasSuffix(file.Name, "_test.go") || strings.Contains(file.Name, "/testdata/")
					if !valid || judgeOwners[file.Name] {
						return nil, fmt.Errorf("Conflicting or invalid independent test path: %s", file.Name)
					}
					judgeOwners[file.Name] = true
				}
				key := module.ID + "/" + id + "/" + step.ID
				sealDoc := struct {
					Key      string        `json:"key"`
					Spec     stepDef       `json:"spec"`
					Sources  []NamedDigest `json:"sources"`
					Contract string        `json:"contract"`
					Judge    []NamedDigest `json:"judge"`
					Assets   []NamedDigest `json:"assets"`
					Rules    string        `json:"rules"`
					Verifier string        `json:"verifier"`
				}{
					Key:      key,
					Spec:     step,
					Sources:  sources,
					Contract: Hash(contract),
					Judge:    entriesOf(judge),
					Assets:   entriesOf(assets),
					Rules:    Hash(rules),
					Verifier: VERIFIER_VERSION,
				}
				sealJSON, err := json.Marshal(sealDoc)
				if err != nil {
					return nil, err
				}
				items = append(items, moduleItem{
					Key:      key,
					Module:   module.ID,
					Batch:    id,
					Spec:     step,
					Seal:     Hash(sealJSON),
					Contract: string(contract),
					Judge:    judge,
					Assets:   assets,
				})
			}
			if cfg.Status == "ready" {
				for _, name := range batch.Outputs {
					covered := false
					for _, step := range cfg.Steps {
						if containsString(step.Outputs, name) {
							covered = true
							break
						}
						for _, asset := range step.Assets {
							if asset.Target == name {
								covered = true
								break
							}
						}
						if covered {
							break
						}
					}
					if !covered {
						return nil, fmt.Errorf("Ready batch does not cover all outputs: %s", id)
					}
				}
			}
		}
	}
	for name := range judgeOwners {
		if _, ok := outputOwners[name]; ok {
			return nil, fmt.Errorf("Candidate can overwrite an independent test: %s", name)
		}
	}
	if workflow.Journal != "" {
		if _, err := RelativeName(workflow.Journal); err != nil {
			return nil, err
		}
		if !regexp.MustCompile(`^\.portsmith/(?:[a-zA-Z0-9_-]+/)*[a-zA-Z0-9_-]+\.json$`).MatchString(workflow.Journal) {
			return nil, errors.New("journal must be a JSON file under .portsmith")
		}
	}
	baseline := []File{}
	if workflow.Baseline != nil {
		if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(workflow.Baseline.Commit) || len(workflow.Baseline.Files) == 0 {
			return nil, errors.New("baseline requires a full commit and non-empty file manifest")
		}
		if workflow.Journal == "" || workflow.Journal == ".portsmith/modules.json" {
			return nil, errors.New("an additive baseline requires a separate journal")
		}
		if _, err := gitRun(context.Background(), project, "merge-base", "--is-ancestor", workflow.Baseline.Commit, "HEAD"); err != nil {
			return nil, err
		}
		treeOutput, err := gitRun(context.Background(), project, "ls-tree", "-r", workflow.Baseline.Commit)
		if err != nil {
			return nil, err
		}
		tree := map[string][3]string{}
		for _, line := range strings.Split(treeOutput, "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) != 2 {
				continue
			}
			meta := strings.Split(parts[0], " ")
			if len(meta) < 3 {
				continue
			}
			tree[parts[1]] = [3]string{meta[0], meta[1], meta[2]}
		}
		seen := map[string]bool{}
		for _, file := range workflow.Baseline.Files {
			if _, err := RelativeName(file.Name); err != nil {
				return nil, err
			}
			invalid := !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(file.SHA256) || seen[file.Name] ||
				!regexp.MustCompile(`^(packages|internal|cmd|docs|examples)/`).MatchString(file.Name)
			for _, part := range strings.Split(file.Name, "/") {
				if strings.HasPrefix(part, ".") {
					invalid = true
				}
			}
			if _, ok := outputOwners[file.Name]; ok {
				invalid = true
			}
			if judgeOwners[file.Name] {
				invalid = true
			}
			if invalid {
				return nil, fmt.Errorf("invalid or overlapping baseline file: %s", file.Name)
			}
			seen[file.Name] = true
			data, err := readCheckedBytes(project, file.Name)
			if err != nil {
				return nil, err
			}
			reserved := false
			for _, part := range strings.Split(file.Name, "/") {
				if strings.HasPrefix(part, "portsmith_judge") || strings.HasPrefix(part, "port_oracle") {
					reserved = true
				}
			}
			if reserved || (strings.HasSuffix(file.Name, "_test.go") && judgeTestNamePattern.Match(data)) {
				return nil, fmt.Errorf("baseline contains a reserved judge; keep it in project integration tests: %s", file.Name)
			}
			object, ok := tree[file.Name]
			sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...))
			blob := hex.EncodeToString(sum[:])
			if !ok || object[0] != "100644" || object[2] != blob || Hash(data) != file.SHA256 {
				return nil, fmt.Errorf("baseline changed: %s", file.Name)
			}
			baseline = append(baseline, File{Name: file.Name, Data: data, SHA256: Hash(data)})
		}
	}
	updateOwners := map[string]string{}
	for _, batch := range plan.Batches {
		for _, name := range batch.Outputs {
			if owner := updateOwners[name]; owner != "" && owner != batch.Module {
				return nil, fmt.Errorf("conflicting update ownership: %s", name)
			}
			updateOwners[name] = batch.Module
		}
	}
	updates, err := inspectUpdates(project, workflow, updateOwners, judgeOwners, baseline)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, item := range items {
		names = append(names, item.Spec.Tests...)
	}
	nameSet := map[string]bool{}
	for _, name := range names {
		if nameSet[name] {
			return nil, errors.New("Independent test names must be unique across steps")
		}
		nameSet[name] = true
	}
	for _, bootstrap := range workflow.Bootstrap {
		for output := range outputOwners {
			if allows(output, []string{bootstrap}) {
				return nil, errors.New("bootstrap must not include product outputs")
			}
		}
	}
	license, err := readCheckedBytes(source, "LICENSE")
	if err != nil {
		return nil, err
	}
	identityDoc := struct {
		Revision string          `json:"revision"`
		Source   string          `json:"source"`
		Modules  []moduleDef     `json:"modules"`
		Batches  []batchIdentity `json:"batches"`
		Runs     string          `json:"runs"`
		Journal  string          `json:"journal,omitempty"`
		Baseline *baselineConfig `json:"baseline,omitempty"`
		Updates  *baselineConfig `json:"updates,omitempty"`
		Rules    string          `json:"rules"`
		License  string          `json:"license"`
	}{
		Revision: plan.Revision,
		Source:   source,
		Modules:  plan.Modules,
		Batches:  make([]batchIdentity, 0, len(plan.Batches)),
		Runs:     workflow.Runs,
		Journal:  workflow.Journal,
		Baseline: workflow.Baseline,
		Updates:  workflow.Updates,
		Rules:    Hash(rules),
		License:  Hash(license),
	}
	for _, batch := range plan.Batches {
		identityDoc.Batches = append(identityDoc.Batches, batchIdentity{ID: batch.ID, Module: batch.Module, DependsOn: batch.DependsOn})
	}
	identityJSON, err := json.Marshal(identityDoc)
	if err != nil {
		return nil, err
	}
	return &moduleInspection{
		Root:     root,
		Plan:     plan,
		Workflow: workflow,
		Project:  project,
		Source:   source,
		Mod:      mod,
		Sum:      sum,
		Items:    items,
		Identity: Hash(identityJSON),
		Baseline: baseline,
		Updates:  updates,
	}, nil
}

type batchIdentity struct {
	ID        string   `json:"id"`
	Module    string   `json:"module"`
	DependsOn []string `json:"dependsOn"`
}

type nextWorkResult struct {
	Module   *moduleDef
	Next     *moduleItem
	Blocked  []blockedBatch
	Complete bool
}

// nextWork selects the next module and the next runnable step inside it.
func nextWork(inspected *moduleInspection, state *moduleState) nextWorkResult {
	done := map[string]bool{}
	for _, step := range state.Steps {
		done[step.Key] = true
	}
	accepted := map[string]bool{}
	for _, module := range state.Modules {
		accepted[module.ID] = true
	}
	var module *moduleDef
	for idx := range inspected.Plan.Modules {
		candidate := &inspected.Plan.Modules[idx]
		if accepted[candidate.ID] {
			continue
		}
		ready := true
		for _, dependency := range candidate.DependsOn {
			if !accepted[dependency] {
				ready = false
				break
			}
		}
		if ready {
			module = candidate
			break
		}
	}
	if module == nil {
		return nextWorkResult{Complete: true, Blocked: []blockedBatch{}}
	}
	batchesByID := map[string]batchDef{}
	for _, batch := range inspected.Plan.Batches {
		batchesByID[batch.ID] = batch
	}
	batchDone := func(id string) bool {
		if inspected.Workflow.Batches[id].Status != "ready" {
			return false
		}
		for _, item := range inspected.Items {
			if item.Batch == id && !done[item.Key] {
				return false
			}
		}
		return true
	}
	var next *moduleItem
	for _, id := range module.Batches {
		for idx := range inspected.Items {
			item := &inspected.Items[idx]
			if item.Batch != id || done[item.Key] {
				continue
			}
			ready := true
			for _, dependency := range batchesByID[item.Batch].DependsOn {
				if !batchDone(dependency) {
					ready = false
					break
				}
			}
			if ready {
				next = item
				break
			}
		}
		if next != nil {
			break
		}
	}
	blocked := []blockedBatch{}
	for _, id := range module.Batches {
		if inspected.Workflow.Batches[id].Status != "ready" {
			blocked = append(blocked, blockedBatch{Batch: id, Reason: inspected.Workflow.Batches[id].Reason})
		}
	}
	complete := true
	for _, id := range module.Batches {
		if !batchDone(id) {
			complete = false
			break
		}
	}
	return nextWorkResult{Module: module, Next: next, Blocked: blocked, Complete: complete}
}

// validDone revalidates every stored checkpoint and committed module before any
// new work can start.
func validDone(ctx context.Context, inspected *moduleInspection, state *moduleState) error {
	if state.Version != 2 || state.Identity != inspected.Identity {
		return errors.New("Source, module structure or rules changed; replan instead of reusing progress")
	}
	if err := validateUpdateWorkspace(inspected, state); err != nil {
		return err
	}
	verified := map[string]bool{}
	for _, step := range state.Steps {
		if verified[step.Key] {
			return errors.New("Duplicate module progress records")
		}
		verified[step.Key] = true
	}
	moduleIDs := map[string]bool{}
	for _, module := range state.Modules {
		if moduleIDs[module.ID] {
			return errors.New("Duplicate module progress records")
		}
		moduleIDs[module.ID] = true
	}
	for _, done := range state.Steps {
		var item *moduleItem
		for idx := range inspected.Items {
			if inspected.Items[idx].Key == done.Key {
				item = &inspected.Items[idx]
				break
			}
		}
		if item == nil || item.Seal != done.Seal {
			return fmt.Errorf("Completed step source, contract or acceptance changed: %s", done.Key)
		}
		var siblings []moduleItem
		for _, candidate := range inspected.Items {
			if candidate.Batch == item.Batch {
				siblings = append(siblings, candidate)
			}
		}
		index := -1
		for i := range siblings {
			if siblings[i].Key == done.Key {
				index = i
				break
			}
		}
		for i := 0; i < index; i++ {
			if !verified[siblings[i].Key] {
				return fmt.Errorf("Cannot insert a new step before a completed step: %s", done.Key)
			}
		}
		for _, batch := range inspected.Plan.Batches {
			if batch.ID != item.Batch {
				continue
			}
			for _, dependency := range batch.DependsOn {
				if inspected.Workflow.Batches[dependency].Status != "ready" {
					return fmt.Errorf("A prerequisite batch of a completed step was extended: %s", done.Key)
				}
				for _, candidate := range inspected.Items {
					if candidate.Batch == dependency && !verified[candidate.Key] {
						return fmt.Errorf("A prerequisite batch of a completed step was extended: %s", done.Key)
					}
				}
			}
		}
		root := filepath.Join(inspected.Project, filepath.FromSlash(done.Task))
		report, err := CurrentVerification(root)
		if err != nil {
			return err
		}
		if report == nil || !report.Current || report.Report.Status != "behavior_verified" || report.Report.Fingerprint != done.Fingerprint {
			return fmt.Errorf("Completed step verification is no longer valid: %s", done.Key)
		}
		if err := assertFiles(filepath.Join(root, "candidate"), done.Files); err != nil {
			return err
		}
	}
	for _, committed := range state.Modules {
		var def *moduleDef
		for idx := range inspected.Plan.Modules {
			if inspected.Plan.Modules[idx].ID == committed.ID {
				def = &inspected.Plan.Modules[idx]
				break
			}
		}
		if def == nil {
			return fmt.Errorf("Unknown committed module: %s", committed.ID)
		}
		for _, batch := range def.Batches {
			changed := inspected.Workflow.Batches[batch].Status != "ready"
			if !changed {
				for _, candidate := range inspected.Items {
					if candidate.Module == committed.ID && !verified[candidate.Key] {
						changed = true
						break
					}
				}
			}
			if changed {
				return fmt.Errorf("Committed module execution scope changed: %s", committed.ID)
			}
		}
		if _, err := gitRun(ctx, inspected.Project, "merge-base", "--is-ancestor", committed.Commit, "HEAD"); err != nil {
			return err
		}
		if err := assertFiles(inspected.Project, committed.Files); err != nil {
			return err
		}
	}
	return nil
}

// migrateModules executes a version-2 workflow.
func migrateModules(ctx context.Context, options MigrationOptions) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	inspected, err := inspectModules(options.Plan)
	if err != nil {
		return nil, err
	}
	control := filepath.Join(inspected.Project, ".portsmith")
	journalRel := ".portsmith/modules.json"
	if inspected.Workflow.Journal != "" {
		journalRel = inspected.Workflow.Journal
	}
	journal := filepath.Join(inspected.Project, filepath.FromSlash(journalRel))
	fresh := func() moduleState {
		return moduleState{Version: 2, Identity: inspected.Identity, Steps: []moduleDone{}, Modules: []moduleCommit{}, Attempts: map[string]int{}}
	}
	load := func() (moduleState, error) {
		exists, err := fileExists(journal)
		if err != nil {
			return moduleState{}, err
		}
		if !exists {
			return fresh(), nil
		}
		return readJSON[moduleState](inspected.Project, journalRel)
	}
	startPolicy := inspected.Workflow.StartPolicy
	if startPolicy == "" {
		startPolicy = "all-prepared"
	}
	blocked := []blockedBatch{}
	readyBatches := 0
	for _, batch := range inspected.Plan.Batches {
		if inspected.Workflow.Batches[batch.ID].Status == "ready" {
			readyBatches++
		} else {
			blocked = append(blocked, blockedBatch{Module: batch.Module, Batch: batch.ID, Reason: inspected.Workflow.Batches[batch.ID].Reason})
		}
	}
	modulePreparation := []map[string]any{}
	for _, module := range inspected.Plan.Modules {
		ready := 0
		for _, batch := range module.Batches {
			if inspected.Workflow.Batches[batch].Status == "ready" {
				ready++
			}
		}
		modulePreparation = append(modulePreparation, map[string]any{
			"id":           module.ID,
			"readyBatches": ready,
			"totalBatches": len(module.Batches),
		})
	}
	preparation := map[string]any{
		"policy":       startPolicy,
		"ready":        len(blocked) == 0,
		"readyBatches": readyBatches,
		"totalBatches": len(inspected.Plan.Batches),
		"modules":      modulePreparation,
	}
	if options.Check || (len(blocked) > 0 && startPolicy == "all-prepared") {
		state, err := load()
		if err != nil {
			return nil, err
		}
		if err := validDone(ctx, inspected, &state); err != nil {
			return nil, err
		}
		next := nextWork(inspected, &state)
		runnable := (len(blocked) == 0 || startPolicy == "available-steps") && (next.Next != nil || (next.Module != nil && next.Complete))
		status := "needs-preparation"
		if next.Module == nil {
			status = "complete"
		} else if runnable {
			if len(blocked) == 0 {
				status = "ready"
			} else {
				status = "partially-ready"
			}
		}
		moduleStatus := []map[string]string{}
		for _, module := range inspected.Plan.Modules {
			moduleStatus = append(moduleStatus, map[string]string{"id": module.ID, "status": moduleStatusOf(&state, module.ID)})
		}
		note := "Preparation status does not mean the Go module is implemented. Use --commit to call the model and save progress."
		if next.Module == nil {
			note = "All planned modules passed acceptance and were committed. No generation is needed."
		} else if len(blocked) > 0 && startPolicy == "all-prepared" {
			note = "The complete plan is not prepared. No model calls, task creation or commits will occur. Preparing the first step does not unblock missing materials; prepare all batches first."
		}
		document := map[string]any{
			"status":        status,
			"canStart":      runnable,
			"preparation":   preparation,
			"modules":       moduleStatus,
			"preparedSteps": len(inspected.Items),
			"verifiedSteps": len(state.Steps),
			"blocked":       blocked,
			"note":          note,
		}
		if runnable && next.Next != nil {
			document["next"] = next.Next.Key
		}
		return json.Marshal(document)
	}
	if !options.Commit {
		return nil, errors.New("Module migration requires --commit, allowing preparation and whole-module commits; it does not push")
	}
	if options.Generate == nil {
		return nil, errors.New("Module migration requires a generator to call the model")
	}
	maxAttempts := options.MaxAttempts
	limit := options.MaxUnits
	if limit == 0 {
		limit = len(inspected.Plan.Modules)
	}
	if maxAttempts < 0 || limit < 1 {
		return nil, errors.New("max-attempts must be non-negative (0 means unlimited); max-units must be positive (v2 counts modules)")
	}
	if err := os.MkdirAll(control, 0o755); err != nil {
		return nil, err
	}
	if _, err := gitRun(ctx, inspected.Project, "check-ignore", journalRel); err != nil {
		return nil, err
	}
	log := options.OnProgress
	if log == nil {
		log = func(string) {}
	}
	runner := &v2Runner{
		ctx:         ctx,
		options:     options,
		inspected:   inspected,
		journal:     journal,
		journalRel:  journalRel,
		log:         log,
		maxAttempts: maxAttempts,
		maxUnits:    limit,
	}
	var result json.RawMessage
	err = WithLock(ctx, control, func() error {
		var runErr error
		result, runErr = runner.run()
		return runErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func moduleStatusOf(state *moduleState, id string) string {
	for _, module := range state.Modules {
		if module.ID == id {
			return "accepted"
		}
	}
	return "pending"
}

// v2Runner owns version-2 transaction state and recovery.
type v2Runner struct {
	ctx         context.Context
	options     MigrationOptions
	inspected   *moduleInspection
	journal     string
	journalRel  string
	log         func(string)
	maxAttempts int
	maxUnits    int
	state       moduleState
}

func (m *v2Runner) checkCancel() error {
	if err := m.ctx.Err(); err != nil {
		return errors.New("Migration cancelled; module candidate and progress preserved")
	}
	return nil
}

func (m *v2Runner) save() error {
	return AtomicJSON(m.journal, m.state)
}

func (m *v2Runner) clean() error {
	names, err := dirtyFiles(m.ctx, m.inspected.Project)
	if err != nil {
		return err
	}
	if len(names) > 0 {
		return fmt.Errorf("Target working tree has unrelated changes: %s", strings.Join(names, ", "))
	}
	return nil
}

func (m *v2Runner) unchanged() error {
	now, err := inspectModules(m.inspected.Root)
	if err != nil {
		return err
	}
	if now.Identity != m.inspected.Identity ||
		!jsonBytesEqual(now.Plan, m.inspected.Plan) ||
		!jsonBytesEqual(now.Workflow, m.inspected.Workflow) ||
		!sealsEqual(now.Items, m.inspected.Items) ||
		Hash(now.Mod) != Hash(m.inspected.Mod) ||
		hashOrEmpty(now.Sum) != hashOrEmpty(m.inspected.Sum) {
		return errors.New("Migration materials changed during execution; stopped. Review materials before rerunning")
	}
	return validateUpdateWorkspace(m.inspected, &m.state)
}

func (m *v2Runner) run() (json.RawMessage, error) {
	if err := os.MkdirAll(filepath.Dir(m.journal), 0o755); err != nil {
		return nil, err
	}
	state, err := m.load()
	if err != nil {
		return nil, err
	}
	m.state = state
	if err := m.checkCancel(); err != nil {
		return nil, err
	}
	if err := validDone(m.ctx, m.inspected, &m.state); err != nil {
		return nil, err
	}
	initialCount := len(m.state.Modules)
	if err := m.recover(); err != nil {
		return nil, err
	}
	changes, err := dirtyFiles(m.ctx, m.inspected.Project)
	if err != nil {
		return nil, err
	}
	var unexpected []string
	for _, name := range changes {
		if !allows(name, m.inspected.Workflow.Bootstrap) {
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) > 0 {
		return nil, fmt.Errorf("Only migration preparation materials are committed automatically; resolve unrelated changes first: %s", strings.Join(unexpected, ", "))
	}
	if len(changes) > 0 {
		if _, err := commitFiles(m.ctx, m.inspected.Project, changes, "chore: prepare module migration inputs"); err != nil {
			return nil, err
		}
		m.log("Migration preparation materials committed")
	}
	if err := m.save(); err != nil {
		return nil, err
	}
	for len(m.state.Modules)-initialCount < m.maxUnits {
		if err := m.checkCancel(); err != nil {
			return nil, err
		}
		if err := m.unchanged(); err != nil {
			return nil, err
		}
		if err := m.clean(); err != nil {
			return nil, err
		}
		next := nextWork(m.inspected, &m.state)
		if next.Module == nil {
			return m.completeResult()
		}
		if next.Next == nil && !next.Complete {
			verifiedSteps := []string{}
			for _, step := range m.state.Steps {
				verifiedSteps = append(verifiedSteps, step.Key)
			}
			return json.Marshal(map[string]any{
				"status":        "needs-preparation",
				"module":        next.Module.ID,
				"verifiedSteps": verifiedSteps,
				"blocked":       next.Blocked,
				"note":          "Accepted steps are preserved in .portsmith. Add the remaining contracts/tests and rerun the same command; completed steps will not be repeated and the module will not be committed prematurely.",
			})
		}
		if next.Next != nil {
			if err := m.runStep(next.Next, next.Module); err != nil {
				return nil, err
			}
			continue
		}
		if err := m.commitModule(next.Module); err != nil {
			return nil, err
		}
	}
	status := "paused-at-limit"
	if len(m.state.Modules) == len(m.inspected.Plan.Modules) {
		status = "complete"
	}
	modules := []map[string]string{}
	for _, module := range m.state.Modules {
		modules = append(modules, map[string]string{"id": module.ID, "commit": module.Commit})
	}
	return json.Marshal(map[string]any{"status": status, "modules": modules})
}

func (m *v2Runner) load() (moduleState, error) {
	exists, err := fileExists(m.journal)
	if err != nil {
		return moduleState{}, err
	}
	if !exists {
		return moduleState{Version: 2, Identity: m.inspected.Identity, Steps: []moduleDone{}, Modules: []moduleCommit{}, Attempts: map[string]int{}}, nil
	}
	return readJSON[moduleState](m.inspected.Project, m.journalRel)
}

func (m *v2Runner) completeResult() (json.RawMessage, error) {
	modules := []map[string]string{}
	for _, module := range m.state.Modules {
		modules = append(modules, map[string]string{"id": module.ID, "commit": module.Commit})
	}
	return json.Marshal(map[string]any{"status": "complete", "modules": modules})
}

func (m *v2Runner) runStep(item *moduleItem, module *moduleDef) error {
	taskRel := path.Join(m.inspected.Workflow.Runs, item.Key)
	root := filepath.Join(m.inspected.Project, filepath.FromSlash(taskRel))
	current := []moduleDone{}
	for _, step := range m.state.Steps {
		if m.itemByKey(step.Key).Module == item.Module {
			current = append(current, step)
		}
	}
	priorItems := []moduleItem{}
	for _, step := range m.state.Steps {
		priorItems = append(priorItems, *m.itemByKey(step.Key))
	}
	frozenAssets := []File{}
	for _, step := range append(append([]moduleItem{}, priorItems...), *item) {
		if step.Module == item.Module {
			frozenAssets = append(frozenAssets, step.Assets...)
		}
	}
	frozenNames := map[string]bool{}
	for _, file := range frozenAssets {
		frozenNames[file.Name] = true
	}
	ownNames := []string{}
	appendUnique := func(value string) {
		if !containsString(ownNames, value) {
			ownNames = append(ownNames, value)
		}
	}
	for _, step := range current {
		for _, file := range step.Files {
			appendUnique(file.Name)
		}
	}
	for _, name := range item.Spec.Outputs {
		appendUnique(name)
	}
	for _, file := range item.Assets {
		appendUnique(file.Name)
	}
	initial := []File{}
	if len(current) > 0 {
		last := current[len(current)-1]
		for _, file := range last.Files {
			if frozenNames[file.Name] {
				continue
			}
			data, err := readCheckedBytes(filepath.Join(m.inspected.Project, filepath.FromSlash(last.Task), "candidate"), file.Name)
			if err != nil {
				return err
			}
			initial = append(initial, File{Name: file.Name, Data: data, SHA256: Hash(data)})
		}
	}
	for _, file := range m.inspected.Updates {
		if containsString(ownNames, file.Name) && !frozenNames[file.Name] && !fileNamed(initial, file.Name) {
			initial = append(initial, file)
		}
	}
	seed := append([]File{}, m.inspected.Baseline...)
	for _, file := range m.inspected.Updates {
		if !containsString(ownNames, file.Name) && !committedFile(m.state, file.Name) {
			seed = append(seed, file)
		}
	}
	seed = append(seed, frozenAssets...)
	for _, committed := range m.state.Modules {
		for _, file := range committed.Files {
			if strings.HasPrefix(file.Name, "migration/results/") {
				continue
			}
			judgeOwned := false
			for _, step := range priorItems {
				for _, judge := range step.Judge {
					if judge.Name == file.Name {
						judgeOwned = true
					}
				}
			}
			if judgeOwned {
				continue
			}
			data, err := readCheckedBytes(m.inspected.Project, file.Name)
			if err != nil {
				return err
			}
			seed = append(seed, File{Name: file.Name, Data: data, SHA256: Hash(data)})
		}
	}
	judges := []File{}
	tests := []string{}
	for _, step := range append(append([]moduleItem{}, priorItems...), *item) {
		judges = append(judges, step.Judge...)
		tests = append(tests, step.Spec.Tests...)
	}
	taskSealDoc := struct {
		Step    string        `json:"step"`
		Initial []NamedDigest `json:"initial"`
		Seed    []NamedDigest `json:"seed"`
		Judges  []NamedDigest `json:"judges"`
		Mod     string        `json:"mod"`
		Sum     string        `json:"sum"`
	}{
		Step:    item.Seal,
		Initial: entriesOf(initial),
		Seed:    entriesOf(seed),
		Judges:  entriesOf(judges),
		Mod:     Hash(m.inspected.Mod),
		Sum:     hashOrEmpty(m.inspected.Sum),
	}
	taskSealJSON, err := json.Marshal(taskSealDoc)
	if err != nil {
		return err
	}
	taskSeal := Hash(taskSealJSON)
	exists, err := fileExists(root)
	if err != nil {
		return err
	}
	if !exists {
		temp := root + ".preparing"
		interrupted, err := fileExists(temp)
		if err != nil {
			return err
		}
		if interrupted {
			return fmt.Errorf("Interrupted preparation directory exists: %s. Confirm the old process has exited, then move it aside before continuing", temp)
		}
		writable := []string{}
		for _, name := range ownNames {
			if !frozenNames[name] {
				writable = append(writable, name)
			}
		}
		writable = append(writable, "NOTES.md")
		race := false
		for _, step := range append(append([]moduleItem{}, priorItems...), *item) {
			if step.Spec.Race {
				race = true
			}
		}
		goal := fmt.Sprintf("Module %s, step %s.\n%s\nRequired outputs: %s. Earlier files in this module may be updated for integration, but cumulative acceptance must pass. Do not claim the entire module is complete.",
			item.Module, item.Key, item.Spec.Goal, strings.Join(item.Spec.Outputs, ", "))
		prepareOptions := PrepareOptions{
			Source:             m.inspected.Source,
			Out:                temp,
			Files:              item.Spec.Sources,
			Revision:           m.inspected.Plan.Revision,
			Goal:               goal,
			Rules:              filepath.Join(m.inspected.Root, "RULEBOOK.md"),
			GoMod:              filepath.Join(m.inspected.Project, "go.mod"),
			Unit:               item.Key,
			PlanDigest:         taskSeal,
			Contract:           item.Contract,
			JudgeFiles:         judges,
			RequiredJudgeTests: tests,
			Race:               race,
			WritableFiles:      writable,
			Seed:               seed,
			Initial:            initial,
			ModuleTask:         true,
		}
		if m.inspected.Sum != nil {
			prepareOptions.GoSum = filepath.Join(m.inspected.Project, "go.sum")
		}
		if _, err := PrepareTask(m.ctx, prepareOptions); err != nil {
			_ = os.RemoveAll(temp)
			return err
		}
		if err := os.Rename(temp, root); err != nil {
			_ = os.RemoveAll(temp)
			return err
		}
	}
	return WithLock(m.ctx, root, func() error {
		return m.exchangeStep(item, module, taskRel, root, taskSeal, ownNames, seed)
	})
}

func (m *v2Runner) itemByKey(key string) *moduleItem {
	for idx := range m.inspected.Items {
		if m.inspected.Items[idx].Key == key {
			return &m.inspected.Items[idx]
		}
	}
	return nil
}

func (m *v2Runner) exchangeStep(item *moduleItem, module *moduleDef, taskRel, root, taskSeal string, ownNames []string, seed []File) error {
	_, _, task, err := loadTask(root)
	if err != nil {
		return err
	}
	if task.PlanDigest != taskSeal {
		return fmt.Errorf("Materials or dependencies changed for active step %s; review and move aside its unfinished task directory before retrying. Accepted steps are preserved", item.Key)
	}
	verification, err := CurrentVerification(root)
	if err != nil {
		return err
	}
	feedback := ""
	validate := func() (*CurrentVerificationResult, error) {
		files, err := CandidateFiles(root)
		if err != nil {
			return nil, err
		}
		for _, name := range ownNames {
			found := false
			for _, file := range files {
				if file.Name == name {
					found = true
					break
				}
			}
			if !found {
				return nil, errors.New("Candidate is missing required outputs")
			}
		}
		allowed := map[string]bool{"NOTES.md": true, "go.mod": true, "LICENSE": true}
		for _, name := range ownNames {
			allowed[name] = true
		}
		for _, file := range seed {
			allowed[file.Name] = true
		}
		if m.inspected.Sum != nil {
			allowed["go.sum"] = true
		}
		for _, file := range files {
			if !allowed[file.Name] {
				return nil, errors.New("Candidate contains unauthorized files")
			}
		}
		if _, err := VerifyPort(m.ctx, root, m.options.Download); err != nil {
			return nil, err
		}
		return CurrentVerification(root)
	}
	if verification == nil || !verification.Current || verification.Report.Status != "behavior_verified" {
		if result, err := validate(); err != nil {
			feedback = err.Error()
		} else {
			verification = result
			if result != nil && result.Current && result.Report.Status == "behavior_verified" {
				feedback = ""
			} else {
				feedback = "Previous verification failed: " + reportStatus(result) + ". Repair using verification.json; do not modify frozen files."
			}
		}
	}
	attempt := 0
	for verification == nil || !verification.Current || verification.Report.Status != "behavior_verified" {
		if m.maxAttempts > 0 && attempt >= m.maxAttempts {
			report := "No valid verification report yet"
			if verification != nil && verification.Current {
				report = verificationDiagnostics(verification.Report, 2000)
			} else if feedback != "" {
				report = feedback
			}
			return fmt.Errorf("%s reached the limit of %d generation/repair attempts for this run; candidate and progress preserved. Rerun to continue.\n%s\nReport: %s",
				item.Key, m.maxAttempts, report, filepath.Join(root, "verification.json"))
		}
		if err := m.checkCancel(); err != nil {
			return err
		}
		m.state.Attempts[item.Key]++
		attempt++
		if err := m.save(); err != nil {
			return err
		}
		m.log(fmt.Sprintf("%s generation/repair %d", item.Key, m.state.Attempts[item.Key]))
		generated, err := m.options.Generate(m.ctx, root, feedback)
		if err != nil {
			return err
		}
		if err := m.checkCancel(); err != nil {
			return err
		}
		if modelRunFailed(generated.Status) {
			return fmt.Errorf("Model run failed: %s; details: %s; candidate preserved", valueOr(generated.Error, generated.Status), filepath.Join(root, "last-run.json"))
		}
		result, err := validate()
		if err != nil {
			verification = nil
			feedback = err.Error()
			continue
		}
		verification = result
		if result != nil && result.Current && result.Report.Status == "behavior_verified" {
			feedback = ""
		} else {
			feedback = "Cumulative verification failed; read diagnostics and repair without modifying frozen judges"
		}
	}
	if err := m.checkCancel(); err != nil {
		return err
	}
	if err := m.unchanged(); err != nil {
		return err
	}
	files, err := CandidateFiles(root)
	if err != nil {
		return err
	}
	checkpoint := []NamedDigest{}
	for _, file := range files {
		if containsString(ownNames, file.Name) {
			checkpoint = append(checkpoint, NamedDigest{Name: file.Name, SHA256: file.SHA256})
		}
	}
	fingerprint, err := Fingerprint(root)
	if err != nil {
		return err
	}
	m.state.Steps = append(m.state.Steps, moduleDone{
		Key:         item.Key,
		Seal:        item.Seal,
		Task:        taskRel,
		Fingerprint: fingerprint,
		Files:       checkpoint,
	})
	if err := m.save(); err != nil {
		return err
	}
	m.log(fmt.Sprintf("%s passed cumulative acceptance and was saved; module not yet committed", item.Key))
	return nil
}

func (m *v2Runner) commitModule(module *moduleDef) error {
	var last *moduleDone
	for idx := range m.state.Steps {
		step := &m.state.Steps[idx]
		if owner := m.itemByKey(step.Key); owner != nil && owner.Module == module.ID {
			last = step
		}
	}
	if last == nil {
		return fmt.Errorf("No checkpoint for module %s", module.ID)
	}
	root := filepath.Join(m.inspected.Project, filepath.FromSlash(last.Task))
	_, _, task, err := loadTask(root)
	if err != nil {
		return err
	}
	if task.GoModSHA256 != Hash(m.inspected.Mod) || task.GoSumSHA256 != hashOrEmpty(m.inspected.Sum) {
		return errors.New("Dependencies changed after the last step; add a module regression step instead of committing with old acceptance evidence")
	}
	report, err := VerifyPort(m.ctx, root, m.options.Download)
	if err != nil {
		return err
	}
	if report.Status != "behavior_verified" {
		return errors.New("Final cumulative module verification failed; no commit created")
	}
	if err := m.checkCancel(); err != nil {
		return err
	}
	if err := m.unchanged(); err != nil {
		return err
	}
	files := []File{}
	for _, entry := range last.Files {
		data, err := readCheckedBytes(filepath.Join(root, "candidate"), entry.Name)
		if err != nil {
			return err
		}
		files = append(files, File{Name: entry.Name, Data: data, SHA256: Hash(data)})
	}
	for _, item := range m.inspected.Items {
		if item.Module == module.ID {
			files = append(files, item.Judge...)
		}
	}
	steps := []moduleDone{}
	for _, step := range m.state.Steps {
		if strings.HasPrefix(step.Key, module.ID+"/") {
			steps = append(steps, step)
		}
	}
	planRaw, err := readCheckedBytes(m.inspected.Root, "plan.json")
	if err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(map[string]any{
		"module":       module.ID,
		"upstream":     m.inspected.Plan.Revision,
		"steps":        steps,
		"verification": report,
		"planSha256":   Hash(planRaw),
	}, "", "  ")
	if err != nil {
		return err
	}
	receiptData := append(receipt, '\n')
	files = append(files, File{Name: "migration/results/" + module.ID + ".json", Data: receiptData, SHA256: Hash(receiptData)})
	before := []NamedDigest{}
	changed := []File{}
	for _, file := range files {
		exists, err := fileExists(filepath.Join(m.inspected.Project, filepath.FromSlash(file.Name)))
		if err != nil {
			return err
		}
		if exists {
			original := findFile(m.inspected.Updates, file.Name)
			if original == nil {
				return fmt.Errorf("Integration refuses to overwrite an existing file: %s", file.Name)
			}
			data, err := readCheckedBytes(m.inspected.Project, file.Name)
			if err != nil {
				return err
			}
			if Hash(data) != original.SHA256 {
				return fmt.Errorf("Update target changed: %s", file.Name)
			}
			if Hash(data) == file.SHA256 {
				continue
			}
			before = append(before, NamedDigest{Name: file.Name, SHA256: original.SHA256})
		}
		changed = append(changed, file)
	}
	files = changed
	stagingRel := path.Join(m.inspected.Workflow.Runs, module.ID+"-integration")
	staging := filepath.Join(m.inspected.Project, filepath.FromSlash(stagingRel))
	if exists, err := fileExists(staging); err != nil {
		return err
	} else if exists {
		if err := os.RemoveAll(staging); err != nil {
			return err
		}
	}
	if err := copyFiles(staging, files); err != nil {
		return err
	}
	head, err := gitRun(m.ctx, m.inspected.Project, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	m.state.Pending = &modulePending{
		Module:      module.ID,
		Base:        head,
		Files:       entriesOf(files),
		Message:     "feat: port " + module.ID + "\n\nPortsmith-Module: " + m.inspected.Identity + "\nPortsmith-Candidate: " + report.Fingerprint,
		Task:        last.Task,
		Fingerprint: report.Fingerprint,
		Staging:     stagingRel,
		Before:      before,
	}
	if err := m.save(); err != nil {
		return err
	}
	return m.recover()
}

func (m *v2Runner) recover() error {
	pending := m.state.Pending
	if pending == nil {
		return nil
	}
	staging := filepath.Join(m.inspected.Project, filepath.FromSlash(pending.Staging))
	staged, err := snapshotFiles(staging, 0, 0, nil)
	if err != nil {
		return err
	}
	if !digestsEqual(sortedDigests(entriesOf(staged)), sortedDigests(pending.Files)) {
		return errors.New("Module integration staging was modified")
	}
	head, err := gitRun(m.ctx, m.inspected.Project, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	accepted := head
	if head != pending.Base {
		parent, err := gitRun(m.ctx, m.inspected.Project, "rev-parse", "HEAD^")
		if err != nil {
			return err
		}
		message, err := gitRun(m.ctx, m.inspected.Project, "log", "-1", "--format=%B")
		if err != nil {
			return err
		}
		if parent != pending.Base || message != pending.Message {
			return errors.New("HEAD changed after interruption; state preserved")
		}
		changedOutput, err := gitRun(m.ctx, m.inspected.Project, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
		if err != nil {
			return err
		}
		changed := []string{}
		for _, name := range strings.Split(changedOutput, "\n") {
			if name != "" {
				changed = append(changed, name)
			}
		}
		sortStrings(changed)
		expected := make([]string, 0, len(pending.Files))
		for _, file := range pending.Files {
			expected = append(expected, file.Name)
		}
		sortStrings(expected)
		if !stringSlicesEqual(changed, expected) {
			return errors.New("Recovery commit contains unexpected files")
		}
		if err := m.clean(); err != nil {
			return err
		}
		if err := assertFiles(m.inspected.Project, pending.Files); err != nil {
			return err
		}
	} else {
		dirty, err := dirtyFiles(m.ctx, m.inspected.Project)
		if err != nil {
			return err
		}
		allowed := map[string]bool{}
		for _, file := range pending.Files {
			allowed[file.Name] = true
		}
		for _, name := range dirty {
			if !allowed[name] {
				return errors.New("Resolve unrelated changes before resuming")
			}
		}
		for _, file := range staged {
			destination := filepath.Join(m.inspected.Project, filepath.FromSlash(file.Name))
			exists, err := fileExists(destination)
			if err != nil {
				return err
			}
			if exists {
				data, err := readCheckedBytes(m.inspected.Project, file.Name)
				if err != nil {
					return err
				}
				if Hash(data) != file.SHA256 {
					old := digestNamed(pending.Before, file.Name)
					original := findFile(m.inspected.Updates, file.Name)
					if old == "" || original == nil || old != original.SHA256 || Hash(data) != old {
						return fmt.Errorf("Recovery refuses to overwrite user changes: %s", file.Name)
					}
					if err := replaceProjectFile(m.inspected.Project, file); err != nil {
						return err
					}
				}
			} else if err := copyFiles(m.inspected.Project, []File{file}); err != nil {
				return err
			}
		}
		root := filepath.Join(m.inspected.Project, filepath.FromSlash(pending.Task))
		verification, err := CurrentVerification(root)
		if err != nil {
			return err
		}
		if verification == nil || !verification.Current || verification.Report.Status != "behavior_verified" || verification.Report.Fingerprint != pending.Fingerprint {
			return errors.New("Verification became invalid before module commit")
		}
		_, _, task, err := loadTask(root)
		if err != nil {
			return err
		}
		if err := m.checkCancel(); err != nil {
			return err
		}
		result := executeGo(m.ctx, m.inspected.Project, nil, m.options.Download, task.Race)
		if err := AtomicJSON(filepath.Join(root, "integration-tests.json"), result); err != nil {
			return err
		}
		counts := testResults(result, "TestPortsmithJudge")
		if !succeeded(result) || counts.Skipped > 0 || !requiredTestsPassed(task, counts.PassedNames) {
			return errors.New("Project integration tests failed; no commit created, state preserved")
		}
		if err := m.checkCancel(); err != nil {
			return err
		}
		if err := m.unchanged(); err != nil {
			return err
		}
		if err := assertFiles(m.inspected.Project, pending.Files); err != nil {
			return err
		}
		names := make([]string, 0, len(pending.Files))
		for _, file := range pending.Files {
			names = append(names, file.Name)
		}
		accepted, err = commitFiles(m.ctx, m.inspected.Project, names, pending.Message)
		if err != nil {
			return err
		}
		if err := m.clean(); err != nil {
			return err
		}
		if err := assertFiles(m.inspected.Project, pending.Files); err != nil {
			return err
		}
	}
	m.state.Modules = append(m.state.Modules, moduleCommit{ID: pending.Module, Commit: accepted, Files: pending.Files})
	m.state.Pending = nil
	if err := m.save(); err != nil {
		return err
	}
	m.log(fmt.Sprintf("%s module accepted and committed as %s", pending.Module, shortCommit(accepted)))
	return nil
}

func jsonBytesEqual(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(left) == string(right)
}

func sealsEqual(a, b []moduleItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].Seal != b[i].Seal {
			return false
		}
	}
	return true
}
