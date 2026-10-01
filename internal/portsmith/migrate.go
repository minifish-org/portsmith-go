// migrate.go ports src/migrate.ts: version-1 workflow execution, transactional
// per-unit integration, Git recovery, resumable receipts and strict frozen
// material checks. It shares the low-level filesystem, process and workspace
// helpers with the rest of the package and never pushes.
package portsmith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MigrationOptions configures a version-1 or version-2 migration run. A nil
// Generate is only valid for --check preflight.
type MigrationOptions struct {
	Plan                    string
	Commit, Check, Download bool
	MaxAttempts, MaxUnits   int
	Generate                func(context.Context, string, string) (RunReport, error)
	OnProgress              func(string)
}

// unitConfig is one version-1 workflow unit entry (`Configuration` in
// src/migrate.ts).
type unitConfig struct {
	Contract string   `json:"contract"`
	Judge    string   `json:"judge"`
	Outputs  []string `json:"outputs"`
	Tests    []string `json:"tests"`
	Race     bool     `json:"race,omitempty"`
}

// migrationWorkflow is the version-1 `workflow.json` document.
type migrationWorkflow struct {
	Version   int                   `json:"version"`
	Project   string                `json:"project"`
	Runs      string                `json:"runs"`
	Bootstrap []string              `json:"bootstrap"`
	Units     map[string]unitConfig `json:"units"`
}

// inspectedUnit bundles one selected plan unit with its frozen materials.
type inspectedUnit struct {
	Unit     Unit
	Spec     unitConfig
	Contract string
	Judge    []File
}

// migrationInspection is the fully validated version-1 execution snapshot.
type migrationInspection struct {
	PlanRoot string
	Project  string
	Config   migrationWorkflow
	Plan     Plan
	Units    []inspectedUnit
	Digest   string
	Mod      []byte
	Sum      []byte
}

// MigrationPending is the version-1 transactional integration record.
type MigrationPending struct {
	ID          string        `json:"id"`
	Base        string        `json:"base"`
	Fingerprint string        `json:"fingerprint"`
	Files       []NamedDigest `json:"files"`
	Message     string        `json:"message"`
}

// MigrationCompleted is one committed version-1 unit.
type MigrationCompleted struct {
	ID     string        `json:"id"`
	Commit string        `json:"commit"`
	Files  []NamedDigest `json:"files"`
}

// MigrationState is the version-1 migration journal.
type MigrationState struct {
	Version   int                  `json:"version"`
	Digest    string               `json:"digest"`
	Completed []MigrationCompleted `json:"completed"`
	Pending   *MigrationPending    `json:"pending,omitempty"`
	Attempts  map[string]int       `json:"attempts"`
	Error     string               `json:"error,omitempty"`
}

var (
	v1JudgeNamePattern = regexp.MustCompile(`^TestPortsmithJudge\w+$`)
	v1RenamePattern    = regexp.MustCompile(`^[RC]|^.[RC]`)
)

// gitRun executes git with the verification allowlist and returns trimmed
// stdout. A non-zero exit is an error, matching `execFile` rejection.
func gitRun(ctx context.Context, root string, args ...string) (string, error) {
	result := Execute(ctx, ProcessOptions{Command: "git", Args: args, Cwd: root})
	if !succeeded(result) {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(result.Log))
	}
	return strings.TrimRightFunc(result.Log, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ' ' || r == '\t'
	}), nil
}

// fileExists reports whether a path exists without following a final symlink.
func fileExists(name string) (bool, error) {
	if _, err := os.Lstat(name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// dirtyFiles returns the working-tree paths reported by porcelain status. A
// rename is rejected instead of being guessed.
func dirtyFiles(ctx context.Context, root string) ([]string, error) {
	data, err := gitRun(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var entries []string
	for _, entry := range strings.Split(data, "\x00") {
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	for _, entry := range entries {
		if v1RenamePattern.MatchString(entry) {
			return nil, errors.New("Resolve Git renames before continuing the migration")
		}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if len(entry) < 3 {
			continue
		}
		names = append(names, entry[3:])
	}
	return names, nil
}

// allows mirrors the upstream root-prefix helper.
func allows(name string, roots []string) bool {
	for _, root := range roots {
		if name == root || strings.HasPrefix(name, root+"/") {
			return true
		}
	}
	return false
}

// commitFiles stages exactly names and creates one commit, refusing an empty
// commit. It returns the new HEAD.
func commitFiles(ctx context.Context, root string, names []string, message string) (string, error) {
	if len(names) == 0 {
		return "", errors.New("Refusing an empty commit")
	}
	if _, err := gitRun(ctx, root, append([]string{"add", "--"}, names...)...); err != nil {
		return "", err
	}
	args := append([]string{"commit", "--only", "-m", message, "--"}, names...)
	if _, err := gitRun(ctx, root, args...); err != nil {
		return "", err
	}
	return gitRun(ctx, root, "rev-parse", "HEAD")
}

// assertFiles re-verifies frozen or integrated bytes.
func assertFiles(root string, files []NamedDigest) error {
	for _, file := range files {
		data, err := readCheckedBytes(root, file.Name)
		if err != nil {
			return err
		}
		if Hash(data) != file.SHA256 {
			return fmt.Errorf("Frozen or integrated file changed: %s", file.Name)
		}
	}
	return nil
}

func readCheckedBytes(root, name string) ([]byte, error) {
	file, err := CheckedFile(root, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(file)
}

// entriesOf computes `{name, sha256}` records for in-memory files.
func entriesOf(files []File) []NamedDigest {
	out := make([]NamedDigest, 0, len(files))
	for _, file := range files {
		out = append(out, NamedDigest{Name: file.Name, SHA256: Hash(file.Data)})
	}
	return out
}

func sortedDigests(files []NamedDigest) []NamedDigest {
	out := append([]NamedDigest{}, files...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Name > out[j].Name; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func digestsEqual(a, b []NamedDigest) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].SHA256 != b[i].SHA256 {
			return false
		}
	}
	return true
}

func testFunctionPresent(text, name string) bool {
	return regexp.MustCompile(`func\s+` + regexp.QuoteMeta(name) + `\s*\(`).MatchString(text)
}

// inspectMigration fully validates a version-1 workflow before any model call
// or commit. It is intentionally strict: every contract, judge and output path
// is checked here, not lazily during execution.
func inspectMigration(planInput string) (*migrationInspection, error) {
	absolute, err := filepath.Abs(planInput)
	if err != nil {
		return nil, err
	}
	planRoot, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	config, err := readJSON[migrationWorkflow](planRoot, "workflow.json")
	if err != nil {
		return nil, err
	}
	if config.Version != 1 || config.Project == "" || config.Bootstrap == nil || config.Units == nil {
		return nil, errors.New("Invalid workflow.json")
	}
	projectInput := config.Project
	if !filepath.IsAbs(projectInput) {
		projectInput = filepath.Join(planRoot, projectInput)
	}
	project, err := filepath.EvalSymlinks(projectInput)
	if err != nil {
		return nil, err
	}
	if _, err := RelativeName(config.Runs); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(config.Runs, ".portsmith/") {
		return nil, errors.New("Task directories must be under .portsmith/")
	}
	for _, name := range config.Bootstrap {
		if _, err := RelativeName(name); err != nil {
			return nil, err
		}
		if strings.HasPrefix(name, ".git/") || name == ".git" || strings.HasPrefix(name, ".portsmith") || strings.HasPrefix(name, ".env") {
			return nil, errors.New("bootstrap must not contain internal state or credentials")
		}
	}
	top, err := gitRun(context.Background(), project, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if top != project {
		return nil, errors.New("project must be the target Git repository root")
	}
	plan, err := loadPlan(planRoot)
	if err != nil {
		return nil, err
	}
	mod, err := readCheckedBytes(project, "go.mod")
	if err != nil {
		return nil, err
	}
	fixtureMod, err := readCheckedBytes(planRoot, "go.mod")
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
	workflowRaw, err := readCheckedBytes(planRoot, "workflow.json")
	if err != nil {
		return nil, err
	}
	pairs := [][2]string{
		{"workflow.json", Hash(workflowRaw)},
		{"project/go.mod", Hash(mod)},
		{"fixtures/go.mod", Hash(fixtureMod)},
	}
	if sum != nil {
		pairs = append(pairs, [2]string{"go.sum", Hash(sum)})
	}

	owned := map[string]bool{}
	units := make([]inspectedUnit, 0, len(plan.Units))
	for _, unit := range plan.Units {
		if _, err := selectUnit(planRoot, unit.ID, project); err != nil {
			return nil, err
		}
		spec, ok := config.Units[unit.ID]
		if !ok || len(spec.Outputs) == 0 || len(spec.Tests) == 0 {
			return nil, fmt.Errorf("Missing task configuration: %s", unit.ID)
		}
		if _, err := RelativeName(spec.Contract); err != nil {
			return nil, err
		}
		if _, err := RelativeName(spec.Judge); err != nil {
			return nil, err
		}
		contract, err := readCheckedBytes(planRoot, spec.Contract)
		if err != nil {
			return nil, err
		}
		judge, err := snapshotFiles(filepath.Join(planRoot, filepath.FromSlash(spec.Judge)), 0, 0, nil)
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
		text := testText.String()
		seenTests := map[string]bool{}
		for _, name := range spec.Tests {
			if !v1JudgeNamePattern.MatchString(name) || !testFunctionPresent(text, name) {
				return nil, fmt.Errorf("Missing independent tests: %s/%s", unit.ID, name)
			}
			if seenTests[name] {
				return nil, fmt.Errorf("Duplicate test: %s", unit.ID)
			}
			seenTests[name] = true
		}
		for _, file := range append(append([]string{}, spec.Outputs...), fileNames(judge)...) {
			if _, err := RelativeName(file); err != nil {
				return nil, err
			}
			if !strings.HasSuffix(file, ".go") && !strings.Contains(file, "/testdata/") {
				return nil, fmt.Errorf("Only Go and testdata outputs are allowed: %s", file)
			}
			if !strings.HasPrefix(file, unit.TargetPackage+"/") {
				return nil, fmt.Errorf("Output is outside the task package: %s", file)
			}
			if owned[file] {
				return nil, fmt.Errorf("Task output or judge path conflict: %s", file)
			}
			owned[file] = true
		}
		for _, file := range judge {
			if containsString(spec.Outputs, file.Name) {
				return nil, errors.New("Candidate cannot overwrite a judge")
			}
		}
		hasImplementation := false
		hasCandidateTest := false
		for _, name := range spec.Outputs {
			if strings.HasSuffix(name, "_test.go") {
				hasCandidateTest = true
			} else if strings.HasSuffix(name, ".go") {
				hasImplementation = true
			}
		}
		if !hasImplementation || !hasCandidateTest {
			return nil, fmt.Errorf("Task requires implementation and candidate tests: %s", unit.ID)
		}
		pairs = append(pairs, [2]string{spec.Contract, Hash(contract)})
		for _, file := range judge {
			pairs = append(pairs, [2]string{spec.Judge + "/" + file.Name, Hash(file.Data)})
		}
		units = append(units, inspectedUnit{Unit: unit, Spec: spec, Contract: string(contract), Judge: judge})
	}
	digestDocument := struct {
		Plan     string      `json:"plan"`
		Verifier string      `json:"verifier"`
		Assets   [][2]string `json:"assets"`
	}{Plan: planDigestOrEmpty(planRoot), Verifier: VERIFIER_VERSION, Assets: pairs}
	encoded, err := json.Marshal(digestDocument)
	if err != nil {
		return nil, err
	}
	return &migrationInspection{
		PlanRoot: planRoot,
		Project:  project,
		Config:   config,
		Plan:     plan,
		Units:    units,
		Digest:   Hash(encoded),
		Mod:      mod,
		Sum:      sum,
	}, nil
}

func planDigestOrEmpty(root string) string {
	digest, err := planDigest(root)
	if err != nil {
		return ""
	}
	return digest
}

func fileNames(files []File) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Name)
	}
	return out
}

// Migrate runs the reviewed migration workflow. Version-2 module workflows are
// dispatched to migrateModules.
func Migrate(ctx context.Context, options MigrationOptions) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	header, err := readJSON[struct {
		Version int `json:"version"`
	}](options.Plan, "workflow.json")
	if err != nil {
		return nil, err
	}
	if header.Version == 2 {
		return migrateModules(ctx, options)
	}
	inspected, err := inspectMigration(options.Plan)
	if err != nil {
		return nil, err
	}
	if options.Check {
		units := make([]map[string]any, 0, len(inspected.Units))
		for _, item := range inspected.Units {
			units = append(units, map[string]any{
				"id":      item.Unit.ID,
				"tests":   len(item.Spec.Tests),
				"outputs": item.Spec.Outputs,
			})
		}
		return json.Marshal(map[string]any{
			"status": "ready",
			"units":  units,
			"note":   "Preparation checked; no model calls made and Go behavior is not yet implemented",
		})
	}
	if !options.Commit {
		return nil, errors.New("Automatic integration requires --commit, allowing preparation and per-module commits; it does not push")
	}
	if options.Generate == nil {
		return nil, errors.New("Migration requires a generator to call the model")
	}
	units := inspected.Units
	maxAttempts := options.MaxAttempts
	maxUnits := options.MaxUnits
	if maxUnits == 0 {
		maxUnits = len(units)
	}
	if maxAttempts < 0 || maxUnits < 1 {
		return nil, errors.New("max-attempts must be non-negative (0 means unlimited); max-units must be positive")
	}
	log := options.OnProgress
	if log == nil {
		log = func(string) {}
	}
	control := filepath.Join(inspected.Project, ".portsmith")
	if err := os.MkdirAll(control, 0o755); err != nil {
		return nil, err
	}
	if _, err := gitRun(ctx, inspected.Project, "check-ignore", ".portsmith/migrate.json"); err != nil {
		return nil, err
	}
	runner := &v1Runner{
		ctx:       ctx,
		inspected: inspected,
		options:   options,
		journal:   filepath.Join(control, "migrate.json"),
		log:       log,
		maxUnits:  maxUnits,
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

// v1Runner owns the version-1 transaction state and its recovery routine.
type v1Runner struct {
	ctx       context.Context
	inspected *migrationInspection
	options   MigrationOptions
	journal   string
	log       func(string)
	maxUnits  int
	state     MigrationState
}

func (m *v1Runner) checkCancel() error {
	if err := m.ctx.Err(); err != nil {
		return errors.New("Migration cancelled; progress preserved. Rerun the same command to continue")
	}
	return nil
}

func (m *v1Runner) save() error {
	return AtomicJSON(m.journal, m.state)
}

func (m *v1Runner) ensureClean() error {
	names, err := dirtyFiles(m.ctx, m.inspected.Project)
	if err != nil {
		return err
	}
	if len(names) > 0 {
		return fmt.Errorf("Working tree contains changes outside this transaction: %s", strings.Join(names, ", "))
	}
	return nil
}

func (m *v1Runner) run() (json.RawMessage, error) {
	exists, err := fileExists(m.journal)
	if err != nil {
		return nil, err
	}
	if exists {
		state, err := readJSON[MigrationState](m.inspected.Project, ".portsmith/migrate.json")
		if err != nil {
			return nil, err
		}
		m.state = state
	} else {
		m.state = MigrationState{
			Version:   1,
			Digest:    m.inspected.Digest,
			Completed: []MigrationCompleted{},
			Attempts:  map[string]int{},
		}
	}
	if m.state.Version != 1 || m.state.Digest != m.inspected.Digest {
		return nil, errors.New("Plan, interfaces, tests, dependencies or verifier changed; review existing tasks instead of reusing old execution receipts")
	}
	initialCompleted := len(m.state.Completed)
	if err := m.recover(); err != nil {
		return nil, m.fail(err)
	}
	for _, completed := range m.state.Completed {
		if _, err := gitRun(m.ctx, m.inspected.Project, "merge-base", "--is-ancestor", completed.Commit, "HEAD"); err != nil {
			return nil, m.fail(err)
		}
		if err := assertFiles(m.inspected.Project, completed.Files); err != nil {
			return nil, m.fail(err)
		}
	}
	changes, err := dirtyFiles(m.ctx, m.inspected.Project)
	if err != nil {
		return nil, m.fail(err)
	}
	if len(changes) > 0 && !exists {
		var unexpected []string
		for _, name := range changes {
			if !allows(name, m.inspected.Config.Bootstrap) {
				unexpected = append(unexpected, name)
			}
		}
		if len(unexpected) > 0 {
			return nil, m.fail(fmt.Errorf("The first run only commits preparation files automatically; resolve these changes first: %s", strings.Join(unexpected, ", ")))
		}
		if err := m.checkCancel(); err != nil {
			return nil, m.fail(err)
		}
		if _, err := commitFiles(m.ctx, m.inspected.Project, changes, "chore: prepare Portsmith migration inputs"); err != nil {
			return nil, m.fail(err)
		}
		m.log("Preparation commit created for the migration plan, interfaces and acceptance tests")
	}
	if err := m.ensureClean(); err != nil {
		return nil, m.fail(err)
	}
	if err := m.save(); err != nil {
		return nil, err
	}
	finished := len(m.state.Completed) - initialCompleted
	for len(m.state.Completed) < len(m.inspected.Units) && finished < m.maxUnits {
		if err := m.checkCancel(); err != nil {
			return nil, m.fail(err)
		}
		recheck, err := inspectMigration(m.inspected.PlanRoot)
		if err != nil {
			return nil, m.fail(err)
		}
		if recheck.Digest != m.inspected.Digest {
			return nil, m.fail(errors.New("Migration inputs changed during execution; stopped to avoid mixing snapshots"))
		}
		done := map[string]bool{}
		for _, completed := range m.state.Completed {
			done[completed.ID] = true
		}
		item, found := findV1Unit(m.inspected.Units, done)
		if !found {
			return nil, m.fail(errors.New("No task can advance; check the dependency graph and committed records"))
		}
		prior := make([]inspectedUnit, 0, len(m.inspected.Units))
		priorSet := map[string]bool{}
		for _, candidate := range m.inspected.Units {
			if done[candidate.Unit.ID] {
				prior = append(prior, candidate)
				priorSet[candidate.Unit.ID] = true
			}
		}
		_ = priorSet
		judgeFiles := []File{}
		requiredJudgeTests := []string{}
		for _, previous := range prior {
			judgeFiles = append(judgeFiles, previous.Judge...)
			requiredJudgeTests = append(requiredJudgeTests, previous.Spec.Tests...)
		}
		judgeFiles = append(judgeFiles, item.Judge...)
		requiredJudgeTests = append(requiredJudgeTests, item.Spec.Tests...)
		taskRoot := filepath.Join(m.inspected.Project, filepath.FromSlash(m.inspected.Config.Runs), item.Unit.ID)
		if err := m.prepareUnit(item, prior, judgeFiles, requiredJudgeTests, taskRoot); err != nil {
			return nil, m.fail(err)
		}
		if err := WithLock(m.ctx, taskRoot, func() error {
			return m.exchangeUnit(item, prior, judgeFiles, requiredJudgeTests, taskRoot)
		}); err != nil {
			return nil, m.fail(err)
		}
		finished++
	}
	completed := make([]map[string]string, 0, len(m.state.Completed))
	for _, entry := range m.state.Completed {
		completed = append(completed, map[string]string{"id": entry.ID, "commit": entry.Commit})
	}
	status := "paused-at-limit"
	if len(m.state.Completed) == len(m.inspected.Units) {
		status = "complete"
	}
	return json.Marshal(map[string]any{
		"status":    status,
		"completed": completed,
		"remaining": len(m.inspected.Units) - len(m.state.Completed),
	})
}

func (m *v1Runner) fail(cause error) error {
	m.state.Error = cause.Error()
	if exists, err := fileExists(m.journal); err == nil && exists {
		if saveErr := m.save(); saveErr != nil {
			return saveErr
		}
	}
	return cause
}

func (m *v1Runner) prepareUnit(item inspectedUnit, prior []inspectedUnit, judgeFiles []File, requiredJudgeTests []string, taskRoot string) error {
	exists, err := fileExists(taskRoot)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	temp := taskRoot + ".preparing"
	if interrupted, err := fileExists(temp); err != nil {
		return err
	} else if interrupted {
		return fmt.Errorf("Interrupted preparation directory exists: %s. Confirm the previous process has stopped, then move it aside and retry", temp)
	}
	seed := []File{}
	for _, previous := range prior {
		for _, name := range previous.Spec.Outputs {
			data, err := readCheckedBytes(m.inspected.Project, name)
			if err != nil {
				return err
			}
			seed = append(seed, File{Name: name, Data: data, SHA256: Hash(data)})
		}
	}
	goal := item.Unit.Goal +
		"\nAcceptance:\n" + strings.Join(item.Unit.Acceptance, "\n") +
		"\nRequired output files:\n" + strings.Join(item.Spec.Outputs, "\n") +
		"\nCandidate tests must use the normal Test prefix, not TestPortsmithJudge."
	digest, err := planDigest(m.inspected.PlanRoot)
	if err != nil {
		return err
	}
	race := false
	for _, unit := range append(append([]inspectedUnit{}, prior...), item) {
		if unit.Spec.Race {
			race = true
		}
	}
	source := m.inspected.Plan.Source
	if !filepath.IsAbs(source) {
		source = filepath.Join(m.inspected.Project, filepath.FromSlash(source))
	}
	prepareOptions := PrepareOptions{
		Source:             source,
		Out:                temp,
		Revision:           m.inspected.Plan.Revision,
		Goal:               goal,
		Files:              append(append([]string{}, item.Unit.Files...), item.Unit.References...),
		Rules:              filepath.Join(m.inspected.PlanRoot, "RULEBOOK.md"),
		GoMod:              filepath.Join(m.inspected.Project, "go.mod"),
		JudgeFiles:         judgeFiles,
		Unit:               item.Unit.ID,
		DependsOn:          item.Unit.DependsOn,
		PlanDigest:         digest,
		WritableFiles:      append(append([]string{}, item.Spec.Outputs...), "NOTES.md"),
		Seed:               seed,
		Contract:           item.Contract,
		RequiredJudgeTests: requiredJudgeTests,
		Race:               race,
	}
	if m.inspected.Sum != nil {
		prepareOptions.GoSum = filepath.Join(m.inspected.Project, "go.sum")
	}
	if _, err := PrepareTask(m.ctx, prepareOptions); err != nil {
		_ = os.RemoveAll(temp)
		return err
	}
	if err := os.Rename(temp, taskRoot); err != nil {
		_ = os.RemoveAll(temp)
		return err
	}
	m.log(fmt.Sprintf("%s prepared with %d seed files", item.Unit.ID, len(seed)))
	return nil
}

func (m *v1Runner) exchangeUnit(item inspectedUnit, prior []inspectedUnit, judgeFiles []File, requiredJudgeTests []string, taskRoot string) error {
	_, _, task, err := loadTask(taskRoot)
	if err != nil {
		return err
	}
	digest, err := planDigest(m.inspected.PlanRoot)
	if err != nil {
		return err
	}
	if task.Unit != item.Unit.ID ||
		task.PlanDigest != digest ||
		!stringSlicesEqual(stringList(task.WritableFiles), append(append([]string{}, item.Spec.Outputs...), "NOTES.md")) ||
		!stringSlicesEqual(stringList(task.RequiredJudgeTests), requiredJudgeTests) ||
		task.GoModSHA256 != Hash(m.inspected.Mod) ||
		task.Race != anyRace(append(append([]inspectedUnit{}, prior...), item)) {
		return fmt.Errorf("Task %s was not prepared by the current workflow and cannot be reused", item.Unit.ID)
	}
	expectedJudge := entriesOf(judgeFiles)
	if !digestsEqual(task.JudgeFiles, expectedJudge) {
		return fmt.Errorf("Frozen tests, rules or seed code for %s do not match the current plan", item.Unit.ID)
	}
	expectedSeed := []NamedDigest{}
	for _, previous := range prior {
		for _, name := range previous.Spec.Outputs {
			data, err := readCheckedBytes(m.inspected.Project, name)
			if err != nil {
				return err
			}
			expectedSeed = append(expectedSeed, NamedDigest{Name: name, SHA256: Hash(data)})
		}
	}
	if !digestsEqual(namedDigestList(task.SeedFiles), expectedSeed) {
		return fmt.Errorf("Frozen tests, rules or seed code for %s do not match the current plan", item.Unit.ID)
	}
	rules, err := readCheckedBytes(m.inspected.PlanRoot, "RULEBOOK.md")
	if err != nil {
		return err
	}
	if task.RulesSHA256 != Hash(rules) {
		return fmt.Errorf("Frozen tests, rules or seed code for %s do not match the current plan", item.Unit.ID)
	}
	if task.GoSumSHA256 != hashOrEmpty(m.inspected.Sum) {
		return fmt.Errorf("Frozen tests, rules or seed code for %s do not match the current plan", item.Unit.ID)
	}
	verification, err := CurrentVerification(taskRoot)
	if err != nil {
		return err
	}
	feedback := ""
	attempt := 0
	for verification == nil || !verification.Current || verification.Report.Status != "behavior_verified" {
		if m.options.MaxAttempts > 0 && attempt >= m.options.MaxAttempts {
			report := "No valid verification report yet"
			if verification != nil && verification.Current {
				report = verificationDiagnostics(verification.Report, 2000)
			} else if feedback != "" {
				report = feedback
			}
			return fmt.Errorf("%s reached the limit of %d generation/repair attempts for this run; candidate and progress preserved. Rerun the same command to continue.\n%s\nReport: %s",
				item.Unit.ID, m.options.MaxAttempts, report, filepath.Join(taskRoot, "verification.json"))
		}
		if err := m.checkCancel(); err != nil {
			return err
		}
		m.state.Attempts[item.Unit.ID]++
		attempt++
		if err := m.save(); err != nil {
			return err
		}
		m.log(fmt.Sprintf("%s generation/repair %d", item.Unit.ID, m.state.Attempts[item.Unit.ID]))
		result, err := m.options.Generate(m.ctx, taskRoot, feedback)
		if err != nil {
			return err
		}
		if err := m.checkCancel(); err != nil {
			return err
		}
		if modelRunFailed(result.Status) {
			return fmt.Errorf("%s model run failed: %s; details: %s; candidate and progress preserved",
				item.Unit.ID, valueOr(result.Error, result.Status), filepath.Join(taskRoot, "last-run.json"))
		}
		report, err := m.validateUnit(item, task, taskRoot)
		if err != nil {
			verification = nil
			feedback = err.Error()
			m.log(feedback)
			continue
		}
		verification = report
		if report != nil && report.Current && report.Report.Status == "behavior_verified" {
			feedback = ""
		} else {
			feedback = "Previous verification failed: " + reportStatus(report) + ". Repair using verification.json; do not modify frozen files."
		}
		m.log(fmt.Sprintf("%s verification: %s", item.Unit.ID, reportStatus(report)))
	}
	if err := m.checkCancel(); err != nil {
		return err
	}
	if err := m.ensureClean(); err != nil {
		return err
	}
	recheck, err := inspectMigration(m.inspected.PlanRoot)
	if err != nil {
		return err
	}
	if recheck.Digest != m.inspected.Digest {
		return errors.New("Migration inputs changed after acceptance; refusing integration")
	}
	integration := []File{}
	for _, name := range item.Spec.Outputs {
		data, err := readCheckedBytes(taskRoot, "candidate/"+name)
		if err != nil {
			return err
		}
		integration = append(integration, File{Name: name, Data: data, SHA256: Hash(data)})
	}
	integration = append(integration, item.Judge...)
	candidateFingerprint, err := Fingerprint(taskRoot)
	if err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(map[string]any{
		"unit":         item.Unit.ID,
		"upstream":     m.inspected.Plan.Revision,
		"digest":       m.inspected.Digest,
		"candidate":    candidateFingerprint,
		"verification": verification.Report,
	}, "", "  ")
	if err != nil {
		return err
	}
	integration = append(integration, File{Name: "migration/results/" + item.Unit.ID + ".json", Data: append(receipt, '\n'), SHA256: Hash(append(receipt, '\n'))})
	if notes, err := readCheckedBytes(taskRoot, "candidate/NOTES.md"); err == nil {
		integration = append(integration, File{Name: "migration/results/" + item.Unit.ID + ".md", Data: notes, SHA256: Hash(notes)})
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, file := range integration {
		targetExists, err := fileExists(filepath.Join(m.inspected.Project, filepath.FromSlash(file.Name)))
		if err != nil {
			return err
		}
		if targetExists {
			return fmt.Errorf("Integration refuses to overwrite an existing file: %s", file.Name)
		}
	}
	staging := filepath.Join(taskRoot, "integration")
	if exists, err := fileExists(staging); err != nil {
		return err
	} else if exists {
		if err := os.RemoveAll(staging); err != nil {
			return err
		}
	}
	if err := copyFiles(staging, integration); err != nil {
		return err
	}
	head, err := gitRun(m.ctx, m.inspected.Project, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	m.state.Pending = &MigrationPending{
		ID:          item.Unit.ID,
		Base:        head,
		Fingerprint: candidateFingerprint,
		Files:       entriesOf(integration),
		Message: "feat: port " + item.Unit.ID + "\n\nPortsmith-Plan: " + m.inspected.Digest +
			"\nPortsmith-Candidate: " + candidateFingerprint,
	}
	if err := m.save(); err != nil {
		return err
	}
	return m.recover()
}

func (m *v1Runner) validateUnit(item inspectedUnit, task PortTask, taskRoot string) (*CurrentVerificationResult, error) {
	files, err := CandidateFiles(taskRoot)
	if err != nil {
		return nil, err
	}
	missing := []string{}
	for _, name := range item.Spec.Outputs {
		found := false
		for _, file := range files {
			if file.Name == name {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("Missing required outputs: %s", strings.Join(missing, ", "))
	}
	allowed := map[string]bool{
		"go.mod": true, "LICENSE": true,
	}
	for _, name := range stringList(task.WritableFiles) {
		allowed[name] = true
	}
	for _, seed := range namedDigestList(task.SeedFiles) {
		allowed[seed.Name] = true
	}
	if m.inspected.Sum != nil {
		allowed["go.sum"] = true
	}
	for _, file := range files {
		if !allowed[file.Name] {
			return nil, errors.New("Candidate contains unauthorized outputs")
		}
	}
	if _, err := VerifyPort(m.ctx, taskRoot, m.options.Download); err != nil {
		return nil, err
	}
	return CurrentVerification(taskRoot)
}

func (m *v1Runner) recover() error {
	pending := m.state.Pending
	if pending == nil {
		return nil
	}
	taskRoot := filepath.Join(m.inspected.Project, filepath.FromSlash(m.inspected.Config.Runs), pending.ID)
	staging := filepath.Join(taskRoot, "integration")
	staged, err := snapshotFiles(staging, 0, 0, nil)
	if err != nil {
		return err
	}
	if !digestsEqual(sortedDigests(entriesOf(staged)), sortedDigests(pending.Files)) {
		return errors.New("Integration staging snapshot changed")
	}
	head, err := gitRun(m.ctx, m.inspected.Project, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	var committed string
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
			return errors.New("Git HEAD changed after interruption; state preserved for review")
		}
		if err := m.ensureClean(); err != nil {
			return err
		}
		if err := assertFiles(m.inspected.Project, pending.Files); err != nil {
			return err
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
		committed = head
	} else {
		allowed := map[string]bool{}
		for _, file := range pending.Files {
			allowed[file.Name] = true
		}
		dirty, err := dirtyFiles(m.ctx, m.inspected.Project)
		if err != nil {
			return err
		}
		unexpected := []string{}
		for _, name := range dirty {
			if !allowed[name] {
				unexpected = append(unexpected, name)
			}
		}
		if len(unexpected) > 0 {
			return fmt.Errorf("Resolve unrelated changes before resuming: %s", strings.Join(unexpected, ", "))
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
					return fmt.Errorf("Recovery found user changes and refuses to overwrite them: %s", file.Name)
				}
			} else if err := copyFiles(m.inspected.Project, []File{file}); err != nil {
				return err
			}
		}
		if err := assertFiles(m.inspected.Project, pending.Files); err != nil {
			return err
		}
		if err := m.checkCancel(); err != nil {
			return err
		}
		_, _, task, err := loadTask(taskRoot)
		if err != nil {
			return err
		}
		verified, err := CurrentVerification(taskRoot)
		if err != nil {
			return err
		}
		if verified == nil || !verified.Current || verified.Report.Status != "behavior_verified" || verified.Report.Fingerprint != pending.Fingerprint {
			return errors.New("Verification became invalid before integration")
		}
		integrationTests := executeGo(m.ctx, m.inspected.Project, nil, m.options.Download, task.Race)
		if err := AtomicJSON(filepath.Join(taskRoot, "integration-tests.json"), integrationTests); err != nil {
			return err
		}
		counts := testResults(integrationTests, "TestPortsmithJudge")
		if !succeeded(integrationTests) || counts.Skipped > 0 || !requiredTestsPassed(task, counts.PassedNames) {
			return errors.New("Pith integration tests failed; no commit created. See integration-tests.json; state preserved")
		}
		if err := m.checkCancel(); err != nil {
			return err
		}
		if err := assertFiles(m.inspected.Project, pending.Files); err != nil {
			return err
		}
		names := make([]string, 0, len(pending.Files))
		for _, file := range pending.Files {
			names = append(names, file.Name)
		}
		committed, err = commitFiles(m.ctx, m.inspected.Project, names, pending.Message)
		if err != nil {
			return err
		}
		if err := m.ensureClean(); err != nil {
			return err
		}
		if err := assertFiles(m.inspected.Project, pending.Files); err != nil {
			return err
		}
	}
	m.state.Completed = append(m.state.Completed, MigrationCompleted{ID: pending.ID, Commit: committed, Files: pending.Files})
	m.state.Pending = nil
	m.state.Error = ""
	if err := m.save(); err != nil {
		return err
	}
	m.log(fmt.Sprintf("%s integrated and committed as %s", pending.ID, shortCommit(committed)))
	return nil
}

func findV1Unit(units []inspectedUnit, done map[string]bool) (inspectedUnit, bool) {
	for _, unit := range units {
		if done[unit.Unit.ID] {
			continue
		}
		ready := true
		for _, dependency := range unit.Unit.DependsOn {
			if !done[dependency] {
				ready = false
				break
			}
		}
		if ready {
			return unit, true
		}
	}
	return inspectedUnit{}, false
}

func anyRace(units []inspectedUnit) bool {
	for _, unit := range units {
		if unit.Spec.Race {
			return true
		}
	}
	return false
}

func hashOrEmpty(data []byte) string {
	if data == nil {
		return ""
	}
	return Hash(data)
}

func reportStatus(result *CurrentVerificationResult) string {
	if result == nil {
		return "no valid verification report yet"
	}
	return result.Report.Status
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func modelRunFailed(status string) bool {
	switch status {
	case "model_error", "cancelled", "output_limit", "turn_limit", "timeout":
		return true
	}
	return false
}

func shortCommit(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}
