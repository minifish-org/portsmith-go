// workspace.go ports src/workspace.ts: immutable task snapshots, writable
// candidate roots and change fingerprints. Prepare writes a brand-new task
// directory, LoadTask re-verifies every frozen byte, and the candidate paths
// reject traversal, symbolic links and control-file writes.
package portsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ReferenceRecord is one frozen reference source (`{path, sha256, bytes}`).
type ReferenceRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// NamedDigest is a frozen named file digest (`{name, sha256}`) used for judge
// files and seed files.
type NamedDigest struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// PortTask is the version-1 task manifest written below a prepared root. Its
// JSON field names and ordering follow the upstream `PortTask` object. Optional
// members use omitempty so a fresh task omits the same properties TypeScript
// omits for absent options.
type PortTask struct {
	Version            int               `json:"version"`
	Revision           string            `json:"revision"`
	Goal               string            `json:"goal"`
	Example            string            `json:"example,omitempty"`
	Unit               string            `json:"unit,omitempty"`
	PlanDigest         string            `json:"planDigest,omitempty"`
	DependsOn          []string          `json:"dependsOn"`
	Files              []ReferenceRecord `json:"files"`
	RulesSHA256        string            `json:"rulesSha256"`
	GoModSHA256        string            `json:"goModSha256"`
	GoSumSHA256        string            `json:"goSumSha256,omitempty"`
	JudgeFiles         []NamedDigest     `json:"judgeFiles"`
	WritableFiles      *[]string         `json:"writableFiles,omitempty"`
	SeedFiles          *[]NamedDigest    `json:"seedFiles,omitempty"`
	RequiredJudgeTests *[]string         `json:"requiredJudgeTests,omitempty"`
	Race               bool              `json:"race,omitempty"`
	ModuleTask         bool              `json:"moduleTask,omitempty"`
}

// optionalStrings preserves the distinction between an absent option and an
// explicitly empty list. `omitempty` on a plain slice cannot.
func optionalStrings(values []string) *[]string {
	if values == nil {
		return nil
	}
	copyValues := append([]string{}, values...)
	return &copyValues
}

func stringList(values *[]string) []string {
	if values == nil {
		return nil
	}
	return *values
}

func namedDigestList(values *[]NamedDigest) []NamedDigest {
	if values == nil {
		return nil
	}
	return *values
}

// EVENT_GOAL is the built-in goal used by the demo and event-stream example.
const EVENT_GOAL = `Port the generic EventStream and FIFO queue into package port. AssistantMessageEventStream is out of scope.
Public interface: type StreamItem[T any] struct { Value T; Done bool }
NewEventStream[T any,R any](isComplete func(T)bool, extractResult func(T)R)*EventStream[T,R]
Push(T); Next() <-chan StreamItem[T]; End(*R); Result(context.Context)(R,error)
Next registers a consumer synchronously and returns a channel buffered to one item. Use FIFO, not broadcast. Completion events remain consumable; ignore Push after completion and drain queued events after End.
The first result wins. End(nil) wakes consumers without resolving Result. Result supports cancellation; methods are concurrency-safe.
Read all reference source and tests. Generate event_stream.go, *_test.go and NOTES.md documenting interface mappings, differences and limitations. Preserve the license.`

// PrepareOptions mirrors the `prepareTask` option bag. Empty optional strings
// mean the option is absent. Seed, Initial and JudgeFiles carry in-memory bytes
// so callers can prepare without touching the host filesystem.
type PrepareOptions struct {
	Source, Out, Revision, Goal, Example, Rules, GoMod, GoSum, Judge, Unit, PlanDigest, Contract string
	Files, DependsOn, WritableFiles, RequiredJudgeTests                                          []string
	Seed, Initial, JudgeFiles                                                                    []File
	Race, ModuleTask                                                                             bool
}

var (
	localReplacePattern = regexp.MustCompile(`(?:=>\s*)["']?(?:\.\.?/|/|[A-Za-z]:)`)
	candidateGoPattern  = regexp.MustCompile(`^[a-z0-9_/-]+\.go$`)
	candidateAssetPatt  = regexp.MustCompile(`\.(json|txt|md|yaml|yml|csv)$`)
	packageDeclPattern  = regexp.MustCompile(`(?m)^\s*package\s+[A-Za-z_]\w*\s*(?:\r?\n|;)`)
)

// PrepareTask validates the options, snapshots the source and judge bytes, and
// writes a new task root. An existing root is never overwritten because the
// directory is created with O_EXCL semantics.
func PrepareTask(ctx context.Context, o PrepareOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source, err := filepath.EvalSymlinks(o.Source)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(o.Goal) == "" || len(o.Files) == 0 {
		return "", errors.New("A task goal and reference files are required")
	}

	// `[...new Set([...files, "LICENSE"])]` preserves first-seen order.
	var names []string
	seen := map[string]bool{}
	for _, name := range o.Files {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if !seen["LICENSE"] {
		names = append(names, "LICENSE")
	}
	refs := make([]File, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		file, err := CheckedFile(source, name)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		refs = append(refs, File{Name: name, Data: data, SHA256: Hash(data)})
	}

	rules := []byte(DEFAULT_RULES)
	if o.Rules != "" {
		if rules, err = os.ReadFile(o.Rules); err != nil {
			return "", err
		}
	}

	mod := "module example.com/portsmith-candidate\n\ngo 1.24\n"
	if o.GoMod != "" {
		data, err := os.ReadFile(o.GoMod)
		if err != nil {
			return "", err
		}
		mod = string(data)
	}
	if localReplacePattern.MatchString(mod) {
		return "", errors.New("Isolated candidates do not support local replace directives; use published dependencies or include the required Go source in the candidate")
	}

	var sum []byte
	if o.GoSum != "" {
		if sum, err = os.ReadFile(o.GoSum); err != nil {
			return "", err
		}
	}

	judge := o.JudgeFiles
	judgeProvided := o.JudgeFiles != nil
	if judge == nil && o.Judge != "" {
		judgeRoot, err := filepath.EvalSymlinks(o.Judge)
		if err != nil {
			return "", err
		}
		if judge, err = snapshotFiles(judgeRoot, 0, 0, nil); err != nil {
			return "", err
		}
	}
	if judge == nil {
		judge = []File{}
	}

	for _, name := range o.WritableFiles {
		if _, err := RelativeName(name); err != nil {
			return "", err
		}
	}
	for _, file := range o.Seed {
		if _, err := RelativeName(file.Name); err != nil {
			return "", err
		}
		if containsString(o.WritableFiles, file.Name) {
			return "", errors.New("Seed files cannot also be writable")
		}
	}
	for _, file := range o.Initial {
		if _, err := RelativeName(file.Name); err != nil {
			return "", err
		}
		overwritesSeed := false
		for _, seed := range o.Seed {
			if seed.Name == file.Name {
				overwritesSeed = true
			}
		}
		if !o.ModuleTask || !containsString(o.WritableFiles, file.Name) || overwritesSeed {
			return "", errors.New("Initial module files must be in the writable manifest and must not overwrite seed code")
		}
	}

	if o.Example != "" && len(judge) > 0 {
		return "", errors.New("Choose either the built-in verifier or a custom judge")
	}
	if (o.Judge != "" || judgeProvided) && !judgeHasTest(judge) {
		return "", errors.New("A judge requires independent Go tests with names starting with TestPortsmithJudge")
	}
	for _, file := range judge {
		if file.Name == "go.mod" || file.Name == "go.sum" || file.Name == "LICENSE" {
			return "", errors.New("Judges cannot overwrite dependencies or licenses")
		}
	}

	root, err := filepath.Abs(o.Out)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return "", err
	}
	if err := os.Mkdir(root, 0o777); err != nil {
		return "", err
	}
	if err := os.Mkdir(filepath.Join(root, "candidate"), 0o777); err != nil {
		return "", err
	}
	if err := copyFiles(filepath.Join(root, "references"), refs); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "RULEBOOK.md"), rules, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "candidate", "go.mod"), []byte(mod), 0o644); err != nil {
		return "", err
	}
	if sum != nil {
		if err := os.WriteFile(filepath.Join(root, "candidate", "go.sum"), sum, 0o644); err != nil {
			return "", err
		}
	}
	for _, ref := range refs {
		if ref.Name == "LICENSE" {
			if err := os.WriteFile(filepath.Join(root, "candidate", "LICENSE"), ref.Data, 0o644); err != nil {
				return "", err
			}
		}
	}
	if len(judge) > 0 {
		if err := copyFiles(filepath.Join(root, "judge"), judge); err != nil {
			return "", err
		}
	}
	if len(o.Seed) > 0 {
		if err := copyFiles(filepath.Join(root, "candidate"), o.Seed); err != nil {
			return "", err
		}
	}
	if len(o.Initial) > 0 {
		if err := copyFiles(filepath.Join(root, "candidate"), o.Initial); err != nil {
			return "", err
		}
	}

	goal := o.Goal
	if o.Contract != "" {
		goal += "\nFrozen Go interface and acceptance contract:\n" + o.Contract
	}
	dependsOn := o.DependsOn
	if dependsOn == nil {
		dependsOn = []string{}
	}
	task := PortTask{
		Version:            1,
		Revision:           o.Revision,
		Goal:               goal,
		Example:            o.Example,
		Unit:               o.Unit,
		PlanDigest:         o.PlanDigest,
		DependsOn:          dependsOn,
		Files:              make([]ReferenceRecord, 0, len(refs)),
		RulesSHA256:        Hash(rules),
		GoModSHA256:        Hash([]byte(mod)),
		JudgeFiles:         make([]NamedDigest, 0, len(judge)),
		WritableFiles:      optionalStrings(o.WritableFiles),
		RequiredJudgeTests: optionalStrings(o.RequiredJudgeTests),
		Race:               o.Race,
		ModuleTask:         o.ModuleTask,
	}
	if o.Seed != nil {
		seedFiles := []NamedDigest{}
		task.SeedFiles = &seedFiles
	}
	for _, ref := range refs {
		task.Files = append(task.Files, ReferenceRecord{Path: ref.Name, SHA256: Hash(ref.Data), Bytes: len(ref.Data)})
	}
	if sum != nil {
		task.GoSumSHA256 = Hash(sum)
	}
	for _, file := range judge {
		task.JudgeFiles = append(task.JudgeFiles, NamedDigest{Name: file.Name, SHA256: Hash(file.Data)})
	}
	for _, file := range o.Seed {
		*task.SeedFiles = append(*task.SeedFiles, NamedDigest{Name: file.Name, SHA256: Hash(file.Data)})
	}
	if err := AtomicJSON(filepath.Join(root, "task.json"), task); err != nil {
		return "", err
	}
	return root, nil
}

// judgeHasTest mirrors the upstream suffix check (`endsWith("_test.go")`).
func judgeHasTest(files []File) bool {
	for _, file := range files {
		if strings.HasSuffix(file.Name, "_test.go") {
			return true
		}
	}
	return false
}

// candidateIsReserved reports whether any path segment uses a reserved test
// prefix that the independent verifier owns.
func candidateIsReserved(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, "port_oracle") || strings.HasPrefix(part, "portsmith_judge") {
			return true
		}
	}
	return false
}

// candidateHasHidden reports whether any path segment is hidden.
func candidateHasHidden(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// loadTask resolves root, reads task.json and re-verifies every frozen byte.
// It returns the resolved root, the raw task JSON and the parsed manifest.
func loadTask(rootInput string) (string, json.RawMessage, PortTask, error) {
	var empty PortTask
	root, err := filepath.EvalSymlinks(rootInput)
	if err != nil {
		return "", nil, empty, err
	}
	taskFile, err := CheckedFile(root, "task.json")
	if err != nil {
		return "", nil, empty, err
	}
	raw, err := os.ReadFile(taskFile)
	if err != nil {
		return "", nil, empty, err
	}
	var task PortTask
	if err := json.Unmarshal(raw, &task); err != nil {
		return "", nil, empty, err
	}
	if task.Version != 1 || task.Files == nil || task.RulesSHA256 == "" ||
		task.JudgeFiles == nil || (task.Example != "" && task.Example != "event-stream") {
		return "", nil, empty, errors.New("Invalid task; prepare legacy prototype tasks again")
	}
	for _, ref := range task.Files {
		file, err := CheckedFile(root, "references/"+ref.Path)
		if err != nil {
			return "", nil, empty, err
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", nil, empty, err
		}
		if len(data) != ref.Bytes || Hash(data) != ref.SHA256 {
			return "", nil, empty, fmt.Errorf("Reference snapshot changed: %s", ref.Path)
		}
	}
	configs := [][2]string{
		{"RULEBOOK.md", task.RulesSHA256},
		{"candidate/go.mod", task.GoModSHA256},
	}
	if task.GoSumSHA256 != "" {
		configs = append(configs, [2]string{"candidate/go.sum", task.GoSumSHA256})
	}
	for _, config := range configs {
		file, err := CheckedFile(root, config[0])
		if err != nil {
			return "", nil, empty, err
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", nil, empty, err
		}
		if Hash(data) != config[1] {
			return "", nil, empty, fmt.Errorf("Frozen configuration changed: %s; prepare the task again", config[0])
		}
	}
	for _, judge := range task.JudgeFiles {
		file, err := CheckedFile(root, "judge/"+judge.Name)
		if err != nil {
			return "", nil, empty, err
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", nil, empty, err
		}
		if Hash(data) != judge.SHA256 {
			return "", nil, empty, fmt.Errorf("Judge changed: %s", judge.Name)
		}
	}
	for _, seed := range namedDigestList(task.SeedFiles) {
		file, err := CheckedFile(root, "candidate/"+seed.Name)
		if err != nil {
			return "", nil, empty, err
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", nil, empty, err
		}
		if Hash(data) != seed.SHA256 {
			return "", nil, empty, fmt.Errorf("Seed code changed: %s", seed.Name)
		}
	}
	info, err := os.Lstat(filepath.Join(root, "candidate"))
	if err != nil {
		return "", nil, empty, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", nil, empty, errors.New("Candidate directory cannot be a symbolic link")
	}
	return root, json.RawMessage(raw), task, nil
}

// LoadTask resolves and validates a prepared task, returning its task.json
// bytes on success.
func LoadTask(rootInput string) (json.RawMessage, error) {
	_, raw, _, err := loadTask(rootInput)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// WriteCandidate atomically replaces one candidate file after enforcing the
// writable manifest, reserved-name and control-file rules.
func WriteCandidate(root, name string, content []byte) error {
	if _, err := RelativeName(name); err != nil {
		return err
	}
	_, _, task, err := loadTask(root)
	if err != nil {
		return err
	}
	allowedAsset := task.ModuleTask && containsString(stringList(task.WritableFiles), name) && candidateAssetPatt.MatchString(name)
	if (!candidateGoPattern.MatchString(name) && name != "NOTES.md" && !allowedAsset) || candidateIsReserved(name) {
		return errors.New("Only candidate Go files or NOTES.md may be written; the independent verifier cannot be modified")
	}
	if name == "go.mod" || name == "go.sum" || name == "LICENSE" || candidateHasHidden(name) {
		return errors.New("Cannot write dependencies, licenses or hidden files")
	}
	if task.WritableFiles != nil && !containsString(stringList(task.WritableFiles), name) {
		return fmt.Errorf("Not in the current task's writable manifest: %s", name)
	}
	for _, seed := range namedDigestList(task.SeedFiles) {
		if seed.Name == name {
			return errors.New("Cannot modify accepted seed code")
		}
	}
	for _, judge := range task.JudgeFiles {
		if judge.Name == name {
			return errors.New("Cannot overwrite an independent verifier path")
		}
	}
	if strings.HasSuffix(name, ".go") && !packageDeclPattern.Match(content) {
		return errors.New("Go files must contain a package declaration; write_candidate requires a complete file. Use edit_candidate for partial changes")
	}

	candidate := filepath.Join(root, "candidate")
	parts := strings.Split(name, "/")
	dir := candidate
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Symbolic links are not allowed")
		}
	}
	file := filepath.Join(candidate, filepath.FromSlash(name))
	if info, err := os.Lstat(file); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("Target is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	temp := filepath.Join(root, ".candidate-"+randomToken()+".tmp")
	handle, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := handle.Write(content); err != nil {
		handle.Close()
		os.Remove(temp)
		return err
	}
	if err := handle.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, file); err != nil {
		os.Remove(temp)
		return err
	}
	return nil
}

// EditCandidate applies one exact, unique text replacement by re-reading the
// candidate file and delegating the write to WriteCandidate.
func EditCandidate(root, name, oldText, newText string) error {
	if oldText == "" {
		return errors.New("oldText must not be empty")
	}
	file, err := CheckedFile(filepath.Join(root, "candidate"), name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	text := string(data)
	index := strings.Index(text, oldText)
	if index < 0 || strings.Contains(text[index+1:], oldText) {
		return errors.New("oldText must occur exactly once in the file; read the file again first")
	}
	return WriteCandidate(root, name, []byte(text[:index]+newText+text[index+len(oldText):]))
}

// CandidateFiles snapshots the candidate directory in deterministic order.
func CandidateFiles(root string) ([]File, error) {
	return snapshotFiles(filepath.Join(root, "candidate"), 0, 0, nil)
}

// Fingerprint hashes the task manifest together with the candidate file names
// and digests, so any candidate or manifest change invalidates the receipt.
func Fingerprint(root string) (string, error) {
	_, _, task, err := loadTask(root)
	if err != nil {
		return "", err
	}
	files, err := CandidateFiles(root)
	if err != nil {
		return "", err
	}
	if task.GoSumSHA256 == "" {
		for _, file := range files {
			if file.Name == "go.sum" {
				return "", errors.New("go.sum is not in the frozen dependency manifest; prepare the task again")
			}
		}
	}
	if task.Files == nil {
		task.Files = []ReferenceRecord{}
	}
	if task.JudgeFiles == nil {
		task.JudgeFiles = []NamedDigest{}
	}
	if task.DependsOn == nil {
		task.DependsOn = []string{}
	}
	type fingerprintFile struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	}
	document := struct {
		Task  PortTask          `json:"task"`
		Files []fingerprintFile `json:"files"`
	}{Task: task, Files: make([]fingerprintFile, 0, len(files))}
	for _, file := range files {
		document.Files = append(document.Files, fingerprintFile{Name: file.Name, SHA256: file.SHA256})
	}
	encoded, err := marshalCompact(document)
	if err != nil {
		return "", err
	}
	return Hash(encoded), nil
}

// marshalCompact mirrors `JSON.stringify` without HTML escaping or indentation.
func marshalCompact(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}
