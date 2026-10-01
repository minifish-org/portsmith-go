package portsmith

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type SyncMapping struct {
	Source  string   `json:"source"`
	GoFiles []string `json:"goFiles"`
}
type SyncConfig struct {
	Version    int           `json:"version"`
	Repository string        `json:"repository"`
	Revision   string        `json:"revision"`
	Roots      []string      `json:"roots"`
	Mappings   []SyncMapping `json:"mappings"`
	References []string      `json:"references,omitempty"`
}
type SyncOptions struct {
	Project, Config, Source, Upstream, Out string
	Init                                   bool
}
type SyncChange struct {
	Source  string   `json:"source"`
	Kind    string   `json:"kind"`
	Before  string   `json:"before,omitempty"`
	After   string   `json:"after,omitempty"`
	GoFiles []string `json:"goFiles"`
	Reasons []string `json:"reasons,omitempty"`
}
type SyncReport struct {
	Status          string       `json:"status"`
	Before          string       `json:"before"`
	After           string       `json:"after"`
	Plan            string       `json:"plan,omitempty"`
	Changes         []SyncChange `json:"changes"`
	Unmapped        []string     `json:"unmapped"`
	Note            string       `json:"note"`
	ConfigSHA256    string       `json:"configSha256,omitempty"`
	AffectedSources []string     `json:"affectedSources,omitempty"`
}

func validateSnapshot(ctx context.Context, repo, revision, snapshot string) error {
	tree, e := gitRun(ctx, repo, "ls-tree", "-rz", revision)
	if e != nil {
		return e
	}
	for _, entry := range strings.Split(tree, "\x00") {
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\t", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid Git tree entry")
		}
		meta := strings.Fields(parts[0])
		if len(meta) != 3 {
			return fmt.Errorf("invalid Git metadata")
		}
		if meta[0] != "100644" && meta[0] != "100755" {
			continue
		}
		b, e := readCheckedBytes(snapshot, parts[1])
		if e != nil {
			return e
		}
		h := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(b))), b...))
		if hex.EncodeToString(h[:]) != meta[2] {
			return fmt.Errorf("upstream cache differs from Git: %s", parts[1])
		}
	}
	allowed := map[string]bool{}
	for _, entry := range strings.Split(tree, "\x00") {
		parts := strings.SplitN(entry, "\t", 2)
		if len(parts) == 2 {
			allowed[parts[1]] = true
		}
	}
	return filepath.WalkDir(snapshot, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		name, e := filepath.Rel(snapshot, p)
		if e != nil {
			return e
		}
		name = filepath.ToSlash(name)
		if !allowed[name] && !(strings.HasPrefix(name, "sync-before/") && strings.HasSuffix(name, ".txt")) {
			return fmt.Errorf("unexpected file in upstream cache: %s", name)
		}
		return nil
	})
}

func syncSnapshot(ctx context.Context, project, repo, revision string) (string, error) {
	p := filepath.Join(project, ".cache/portsmith-upstream", revision)
	if _, e := os.Lstat(p); os.IsNotExist(e) {
		if e = exportRevision(ctx, repo, revision, p); e != nil {
			return "", e
		}
	} else if e != nil {
		return "", e
	}
	if e := validateSnapshot(ctx, repo, revision, p); e != nil {
		if repairErr := repairArchiveSnapshot(ctx, repo, revision, p); repairErr != nil {
			return "", repairErr
		}
		if e = validateSnapshot(ctx, repo, revision, p); e != nil {
			return "", e
		}
	}
	return p, nil
}

var fullCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

// InitSync imports reviewed source ownership and finer symbol maps from a
// migrated project. It does not infer semantic equivalence or call a model.
func InitSync(project, config string) error {
	upstream, e := readJSON[struct {
		Repository string `json:"repository"`
		Commit     string `json:"commit"`
	}](project, "migration/upstream.json")
	if e != nil {
		return fmt.Errorf("initialize sync: provide migration/upstream.json or write a reviewed sync config: %w", e)
	}
	if !fullCommit.MatchString(upstream.Commit) || upstream.Repository == "" {
		return fmt.Errorf("invalid migration upstream identity")
	}
	owned := map[string]map[string]bool{}
	roots := map[string]bool{}
	primary := map[string]bool{}
	for _, name := range []string{"migration/plan.json", "migration/sdk/plan.json"} {
		b, e := os.ReadFile(filepath.Join(project, name))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		var p modulePlan
		if e = json.Unmarshal(b, &p); e != nil {
			return e
		}
		if p.Revision != upstream.Commit {
			return fmt.Errorf("migration baselines differ: %s", name)
		}
		for _, batch := range p.Batches {
			for _, source := range append(append([]string{}, batch.Sources...), batch.References...) {
				if !sourceFilePattern.MatchString(source) {
					continue
				}
				if containsString(batch.Sources, source) {
					primary[source] = true
				}
				if owned[source] == nil {
					owned[source] = map[string]bool{}
				}
				parts := strings.Split(source, "/")
				if len(parts) >= 3 && containsString(batch.Sources, source) {
					n := 3
					if parts[1] == "coding-agent" && len(parts) >= 4 {
						n = 4
					}
					roots[strings.Join(parts[:n], "/")] = true
				}
				for _, target := range batch.Outputs {
					if strings.HasSuffix(target, ".go") {
						if _, e := CheckedFile(project, target); e == nil {
							owned[source][target] = true
						}
					}
				}
			}
		}
	}
	fine := map[string]map[string]bool{}
	e = filepath.WalkDir(filepath.Join(project, "packages"), func(p string, d os.DirEntry, e error) error {
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.Contains(d.Name(), "source_map") || !strings.HasSuffix(p, ".json") {
			return nil
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		var records []struct {
			Source  string   `json:"source"`
			GoFile  string   `json:"goFile"`
			GoFiles []string `json:"actualGoFiles"`
		}
		if e = json.Unmarshal(b, &records); e != nil {
			return e
		}
		for _, r := range records {
			if owned[r.Source] == nil {
				continue
			}
			files := append(r.GoFiles, r.GoFile)
			for _, f := range files {
				if f == "" {
					continue
				}
				if _, e := CheckedFile(project, f); e != nil {
					return e
				}
				if fine[r.Source] == nil {
					fine[r.Source] = map[string]bool{}
				}
				fine[r.Source][f] = true
			}
		}
		return nil
	})
	if e != nil {
		return e
	}
	c := SyncConfig{Version: 1, Repository: upstream.Repository, Revision: upstream.Commit, Roots: []string{}, Mappings: []SyncMapping{}}
	for r := range roots {
		c.Roots = append(c.Roots, r)
	}
	sort.Strings(c.Roots)
	for s, targets := range owned {
		if !primary[s] && fine[s] == nil {
			c.References = append(c.References, s)
			continue
		}
		if fine[s] != nil {
			targets = fine[s]
		}
		files := []string{}
		for f := range targets {
			b, e := readCheckedBytes(project, f)
			if e != nil {
				return e
			}
			if strings.HasSuffix(f, "_test.go") && judgeTestNamePattern.Match(b) {
				continue
			}
			files = append(files, f)
		}
		sort.Strings(files)
		c.Mappings = append(c.Mappings, SyncMapping{Source: s, GoFiles: files})
	}
	sort.Strings(c.References)
	sort.Slice(c.Mappings, func(i, j int) bool { return c.Mappings[i].Source < c.Mappings[j].Source })
	if len(c.Mappings) == 0 {
		return fmt.Errorf("no reviewed migration source mappings found")
	}
	if _, e = os.Lstat(config); !os.IsNotExist(e) {
		return fmt.Errorf("sync config already exists or cannot be created: %s", config)
	}
	return AtomicJSON(config, c)
}

func syncRepository(ctx context.Context, project string, c SyncConfig, source string) (string, error) {
	if source != "" {
		root, e := filepath.Abs(source)
		if e != nil {
			return "", e
		}
		if _, e = gitRun(ctx, root, "rev-parse", "--git-dir"); e != nil {
			return "", e
		}
		return root, nil
	}
	mirror := filepath.Join(project, ".cache/portsmith-upstream.git")
	exists, e := fileExists(mirror)
	if e != nil {
		return "", e
	}
	if !exists {
		if c.Repository == "" || strings.HasPrefix(c.Repository, "-") {
			return "", fmt.Errorf("invalid upstream repository")
		}
		if e = os.MkdirAll(filepath.Dir(mirror), 0755); e != nil {
			return "", e
		}
		r := Execute(ctx, ProcessOptions{Command: "git", Args: []string{"clone", "--mirror", "--", c.Repository, mirror}, Cwd: project})
		if !succeeded(r) {
			return "", fmt.Errorf("clone upstream: %s", r.Log)
		}
	} else {
		if _, e = gitRun(ctx, mirror, "fetch", "origin", "--prune"); e != nil {
			return "", e
		}
	}
	return mirror, nil
}

// Export the exact regular Git blobs, bypassing archive attributes such as
// eol, export-subst and export-ignore. One batch process handles the entire tree.
func exportRevision(ctx context.Context, repo, revision, destination string) error {
	tree, e := gitRun(ctx, repo, "ls-tree", "-rz", revision)
	if e != nil {
		return e
	}
	temp := destination + ".preparing"
	if _, e = os.Lstat(temp); !os.IsNotExist(e) {
		return fmt.Errorf("interrupted upstream snapshot exists: %s", temp)
	}
	if e = os.MkdirAll(temp, 0755); e != nil {
		return e
	}
	defer os.RemoveAll(temp)
	c := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	c.Dir = repo
	c.Env = envList(CleanEnv(false, false))
	var stderr bytes.Buffer
	c.Stderr = &stderr
	input, e := c.StdinPipe()
	if e != nil {
		return e
	}
	output, e := c.StdoutPipe()
	if e != nil {
		return e
	}
	if e = c.Start(); e != nil {
		return e
	}
	waited := false
	defer func() {
		if !waited {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	}()
	reader := bufio.NewReader(output)
	for _, entry := range strings.Split(tree, "\x00") {
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\t", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid Git tree entry")
		}
		meta := strings.Fields(parts[0])
		if len(meta) != 3 {
			return fmt.Errorf("invalid Git metadata")
		}
		if meta[0] != "100644" && meta[0] != "100755" {
			continue
		}
		name, e := RelativeName(parts[1])
		if e != nil {
			return e
		}
		if _, e = fmt.Fprintln(input, meta[2]); e != nil {
			return e
		}
		header, e := reader.ReadString('\n')
		if e != nil {
			return fmt.Errorf("read Git batch header: %w", e)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != meta[2] || fields[1] != "blob" {
			return fmt.Errorf("unexpected Git batch response for %s", name)
		}
		size, e := strconv.ParseInt(fields[2], 10, 64)
		if e != nil || size < 0 {
			return fmt.Errorf("invalid Git blob size for %s", name)
		}
		path := filepath.Join(temp, filepath.FromSlash(name))
		if e = os.MkdirAll(filepath.Dir(path), 0755); e != nil {
			return e
		}
		mode := os.FileMode(0644)
		if meta[0] == "100755" {
			mode = 0755
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if e != nil {
			return e
		}
		_, copyErr := io.CopyN(f, reader, size)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		separator, e := reader.ReadByte()
		if e != nil {
			return e
		}
		if separator != '\n' {
			return fmt.Errorf("invalid Git batch separator")
		}
	}
	if e = input.Close(); e != nil {
		return e
	}
	e = c.Wait()
	waited = true
	if e != nil {
		return fmt.Errorf("export upstream blobs: %w %s", e, stderr.String())
	}
	return os.Rename(temp, destination)
}

// Legacy versions used git archive, which can rewrite bytes or omit files.
// Repair only bytes proven to be exactly that archive representation. A third
// representation remains a cache edit and is never overwritten.
func repairArchiveSnapshot(ctx context.Context, repo, revision, snapshot string) error {
	tree, e := gitRun(ctx, repo, "ls-tree", "-rz", revision)
	if e != nil {
		return e
	}
	repairs := []File{}
	for _, entry := range strings.Split(tree, "\x00") {
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\t", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid Git tree entry")
		}
		meta := strings.Fields(parts[0])
		if len(meta) != 3 {
			return fmt.Errorf("invalid Git metadata")
		}
		if meta[0] != "100644" && meta[0] != "100755" {
			continue
		}
		name := parts[1]
		if _, e = RelativeName(name); e != nil {
			return e
		}
		b, readErr := readCheckedBytes(snapshot, name)
		if readErr == nil {
			h := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(b))), b...))
			if hex.EncodeToString(h[:]) == meta[2] {
				continue
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		archived, present, e := archiveFile(ctx, repo, revision, name)
		if e != nil {
			return e
		}
		if (readErr == nil && (!present || !bytes.Equal(b, archived))) || (readErr != nil && present) {
			return fmt.Errorf("upstream cache differs from Git: %s", name)
		}
		raw, e := gitBlob(ctx, repo, revision, name)
		if e != nil {
			return e
		}
		repairs = append(repairs, File{Name: name, Data: raw, SHA256: Hash(raw)})
	}
	for _, f := range repairs {
		if _, e = CheckedFile(snapshot, f.Name); e == nil {
			e = replaceProjectFile(snapshot, f)
		} else if os.IsNotExist(e) {
			e = copyFiles(snapshot, []File{f})
		}
		if e != nil {
			return e
		}
	}
	return nil
}

func archiveFile(ctx context.Context, repo, revision, name string) ([]byte, bool, error) {
	c := exec.CommandContext(ctx, "git", "archive", "--format=tar", revision, "--", name)
	c.Dir = repo
	c.Env = envList(CleanEnv(false, false))
	encoded, e := c.Output()
	if e != nil {
		return nil, false, e
	}
	reader := tar.NewReader(bytes.NewReader(encoded))
	for {
		header, e := reader.Next()
		if e == io.EOF {
			return nil, false, nil
		}
		if e != nil {
			return nil, false, e
		}
		if header.Name == name && header.Typeflag == tar.TypeReg {
			b, e := io.ReadAll(reader)
			return b, true, e
		}
	}
}

func trackedNames(ctx context.Context, repo, revision string) ([]string, error) {
	s, e := gitRun(ctx, repo, "ls-tree", "-rz", "--name-only", revision)
	if e != nil {
		return nil, e
	}
	names := []string{}
	for _, n := range strings.Split(s, "\x00") {
		if n != "" {
			if _, e = RelativeName(n); e != nil {
				return nil, e
			}
			names = append(names, n)
		}
	}
	return names, nil
}

func PrepareSync(ctx context.Context, o SyncOptions) (SyncReport, error) {
	project := o.Project
	if project == "" {
		project = "."
	}
	project, e := filepath.Abs(project)
	if e != nil {
		return SyncReport{}, e
	}
	project, e = filepath.EvalSymlinks(project)
	if e != nil {
		return SyncReport{}, e
	}
	top, e := gitRun(ctx, project, "rev-parse", "--show-toplevel")
	if e != nil {
		return SyncReport{}, e
	}
	if top != project {
		return SyncReport{}, fmt.Errorf("project must be the target Git root")
	}
	config := o.Config
	if config == "" {
		config = "migration/sync.json"
	}
	if !filepath.IsAbs(config) {
		config = filepath.Join(project, config)
	}
	configRelative, e := filepath.Rel(project, config)
	if e != nil {
		return SyncReport{}, e
	}
	configRelative = filepath.ToSlash(configRelative)
	if _, e = RelativeName(configRelative); e != nil {
		return SyncReport{}, e
	}
	if !strings.HasPrefix(configRelative, "migration/") {
		return SyncReport{}, fmt.Errorf("sync config must be under target migration/")
	}
	if e = recoverSyncAdvance(ctx, project); e != nil {
		return SyncReport{}, e
	}
	if o.Init {
		e = InitSync(project, config)
		return SyncReport{Status: "initialized", Changes: []SyncChange{}, Unmapped: []string{}, Note: "Review and commit the imported mappings in " + config}, e
	}
	b, e := os.ReadFile(config)
	if e != nil {
		return SyncReport{}, fmt.Errorf("read sync config (run sync --init first): %w", e)
	}
	var c SyncConfig
	if e = json.Unmarshal(b, &c); e != nil {
		return SyncReport{}, e
	}
	if c.Version != 1 || !fullCommit.MatchString(c.Revision) || !fullCommit.MatchString(o.Upstream) {
		return SyncReport{}, fmt.Errorf("sync requires version 1 config and full 40-character base/upstream commit hashes")
	}
	mappings := map[string][]string{}
	for _, m := range c.Mappings {
		if _, e = RelativeName(m.Source); e != nil {
			return SyncReport{}, e
		}
		if _, ok := mappings[m.Source]; ok {
			return SyncReport{}, fmt.Errorf("duplicate source mapping: %s", m.Source)
		}
		for _, f := range m.GoFiles {
			if e = outputName(f); e != nil {
				return SyncReport{}, e
			}
		}
		mappings[m.Source] = m.GoFiles
	}
	for _, root := range c.Roots {
		if _, e = RelativeName(root); e != nil {
			return SyncReport{}, e
		}
	}
	report := SyncReport{ConfigSHA256: Hash(b), Status: "no_changes", Before: c.Revision, After: o.Upstream, Changes: []SyncChange{}, Unmapped: []string{}, Note: "No model calls made."}
	if c.Revision == o.Upstream {
		return report, nil
	}
	repo, e := syncRepository(ctx, project, c, o.Source)
	if e != nil {
		return report, e
	}
	for _, rev := range []string{c.Revision, o.Upstream} {
		got, e := gitRun(ctx, repo, "rev-parse", "--verify", rev+"^{commit}")
		if e != nil {
			return report, e
		}
		if got != rev {
			return report, fmt.Errorf("upstream hash did not resolve exactly")
		}
	}
	beforeNames, e := trackedNames(ctx, repo, c.Revision)
	if e != nil {
		return report, e
	}
	afterNames, e := trackedNames(ctx, repo, o.Upstream)
	if e != nil {
		return report, e
	}
	afterSet := map[string]bool{}
	for _, n := range afterNames {
		afterSet[n] = true
	}
	beforeSet := map[string]bool{}
	all := map[string]bool{}
	for _, n := range beforeNames {
		beforeSet[n] = true
		all[n] = true
	}
	for _, n := range afterNames {
		all[n] = true
	}
	referenceOnly := map[string]bool{}
	for _, n := range c.References {
		if _, e = RelativeName(n); e != nil {
			return SyncReport{}, e
		}
		referenceOnly[n] = true
	}
	scoped := func(n string) bool {
		if referenceOnly[n] {
			return true
		}
		if _, ok := mappings[n]; ok {
			return true
		}
		for _, r := range c.Roots {
			if n == r || strings.HasPrefix(n, r+"/") {
				return sourceFilePattern.MatchString(n) || strings.HasSuffix(n, ".json")
			}
		}
		return n == "package.json" || strings.HasPrefix(filepath.Base(n), "tsconfig")
	}
	changedGo := map[string]bool{}
	for n := range all {
		if !scoped(n) {
			continue
		}
		var before, after []byte
		if beforeSet[n] {
			before, e = gitBlob(ctx, repo, c.Revision, n)
			if e != nil {
				return report, e
			}
		}
		if afterSet[n] {
			after, e = gitBlob(ctx, repo, o.Upstream, n)
			if e != nil {
				return report, e
			}
		}
		if beforeSet[n] && afterSet[n] && bytes.Equal(before, after) {
			continue
		}
		change := SyncChange{Source: n, Kind: "modified", GoFiles: append([]string{}, mappings[n]...)}
		if beforeSet[n] {
			change.Before = Hash(before)
		} else {
			change.Kind = "added"
		}
		if afterSet[n] {
			change.After = Hash(after)
		} else {
			change.Kind = "deleted"
		}
		if len(change.GoFiles) == 0 && referenceOnly[n] {
			change.Reasons = append(change.Reasons, "Reviewed reference context, not implementation ownership; inspect its relevance before assigning outputs.")
		}
		if len(change.GoFiles) == 0 && !referenceOnly[n] {
			report.Unmapped = append(report.Unmapped, n)
			change.Reasons = append(change.Reasons, "Source has no reviewed Go mapping; assign ownership before execution.")
		}
		for _, f := range change.GoFiles {
			changedGo[f] = true
		}
		report.Changes = append(report.Changes, change)
	}
	sort.Slice(report.Changes, func(i, j int) bool { return report.Changes[i].Source < report.Changes[j].Source })
	sort.Strings(report.Unmapped)
	if len(report.Changes) == 0 {
		return report, nil
	}
	id := "pi-" + o.Upstream[:12]
	out := o.Out
	if out == "" {
		out = filepath.Join(project, "migration/sync", id)
	} else if !filepath.IsAbs(out) {
		out = filepath.Join(project, out)
	}
	out, e = filepath.Abs(out)
	if e != nil {
		return report, e
	}
	report.Plan = out
	report.Status = "needs-preparation"
	report.Note = "Review ownership, behavior changes, contract and independent judges; then execute this plan with migrate --commit. Existing accepted journals are preserved."
	if _, e = os.Lstat(out); e == nil {
		old, e := readJSON[SyncReport](out, "sync-report.json")
		if e != nil {
			return report, e
		}
		if old.Before != c.Revision || old.After != o.Upstream || old.ConfigSHA256 != report.ConfigSHA256 {
			return report, fmt.Errorf("existing sync plan belongs to different revisions")
		}
		snapshot, e := syncSnapshot(ctx, project, repo, old.After)
		if e != nil {
			return report, e
		}
		if e = materializeBefore(ctx, repo, snapshot, old); e != nil {
			return report, e
		}
		if inspected, e := inspectModules(out); e == nil {
			ready := true
			for _, b := range inspected.Workflow.Batches {
				if b.Status != "ready" {
					ready = false
				}
			}
			if ready {
				old.Status = "prepared"
				old.Note = "Reviewed workflow is prepared. No model calls made; use --check or --commit."
			}
		}
		return old, nil
	} else if !os.IsNotExist(e) {
		return report, e
	}
	e = writeSyncDraft(ctx, project, repo, out, id, c, &report, changedGo)
	return report, e
}

func writeSyncDraft(ctx context.Context, project, repo, out, id string, c SyncConfig, report *SyncReport, changedGo map[string]bool) error {
	snapshot, e := syncSnapshot(ctx, project, repo, report.After)
	if e != nil {
		return e
	}
	relOut, e := filepath.Rel(project, out)
	if e != nil {
		return e
	}
	if _, e = RelativeName(filepath.ToSlash(relOut)); e != nil {
		return e
	}
	if !strings.HasPrefix(filepath.ToSlash(relOut), "migration/") {
		return fmt.Errorf("sync output must be a new directory under target migration/")
	}
	if e = materializeBefore(ctx, repo, snapshot, *report); e != nil {
		return e
	}
	sources := []string{}
	for _, change := range report.Changes {
		if change.After != "" {
			sources = append(sources, change.Source)
		}
		if change.Before != "" {
			sources = append(sources, "sync-before/"+report.Before+"/"+change.Source+".txt")
		}
	}
	encoded, e := Analyze(ctx, snapshot)
	if e != nil {
		return e
	}
	var analysis Analysis
	if e = json.Unmarshal(encoded, &analysis); e != nil {
		return e
	}
	oldSnapshot, e := syncSnapshot(ctx, project, repo, report.Before)
	if e != nil {
		return e
	}
	oldRaw, e := Analyze(ctx, oldSnapshot)
	if e != nil {
		return e
	}
	var oldAnalysis Analysis
	if e = json.Unmarshal(oldRaw, &oldAnalysis); e != nil {
		return e
	}
	reverse := map[string][]string{}
	for _, a := range []Analysis{oldAnalysis, analysis} {
		for _, f := range a.Files {
			for _, edge := range f.Imports {
				if edge.Target != "" {
					reverse[edge.Target] = append(reverse[edge.Target], f.Path)
				}
			}
		}
	}
	affected := map[string]bool{}
	queue := []string{}
	for _, change := range report.Changes {
		affected[change.Source] = true
		queue = append(queue, change.Source)
		if strings.HasSuffix(change.Source, ".json") {
			for _, m := range c.Mappings {
				if !affected[m.Source] {
					affected[m.Source] = true
					queue = append(queue, m.Source)
				}
			}
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, dependent := range reverse[n] {
			if !affected[dependent] {
				affected[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	newNames := map[string]bool{}
	for _, f := range analysis.Files {
		newNames[f.Path] = true
	}
	for _, m := range c.Mappings {
		if affected[m.Source] {
			report.AffectedSources = append(report.AffectedSources, m.Source)
			for _, f := range m.GoFiles {
				changedGo[f] = true
			}
			if newNames[m.Source] && !containsString(sources, m.Source) {
				sources = append(sources, m.Source)
			}
		}
	}
	sort.Strings(report.AffectedSources)
	known := map[string]bool{}
	for _, f := range analysis.Files {
		known[f.Path] = true
	}
	for _, n := range sources {
		if !known[n] {
			b, e := readCheckedBytes(snapshot, n)
			if e != nil {
				return e
			}
			analysis.Files = append(analysis.Files, SourceFile{Path: n, SHA256: Hash(b), Lines: strings.Count(string(b), "\n"), Exports: []string{}, Imports: []ImportEdge{}})
		}
	}
	sort.Slice(analysis.Files, func(i, j int) bool { return analysis.Files[i].Path < analysis.Files[j].Path })
	head, e := gitRun(ctx, project, "rev-parse", "HEAD")
	if e != nil {
		return e
	}
	names, e := trackedNames(ctx, project, head)
	if e != nil {
		return e
	}
	outputs := []string{}
	for n := range changedGo {
		outputs = append(outputs, n)
	}
	sort.Strings(outputs)
	baseline := []NamedDigest{}
	updates := []NamedDigest{}
	for _, n := range names {
		if !regexp.MustCompile(`^(packages|internal|cmd|docs|examples)/`).MatchString(n) {
			continue
		}
		if e = outputName(n); e != nil {
			continue
		}
		b, e := readCheckedBytes(project, n)
		if e != nil {
			return e
		}
		gitBytes, e := gitBlob(ctx, project, head, n)
		if e != nil {
			return e
		}
		if !bytes.Equal(b, gitBytes) {
			return fmt.Errorf("target file changed before sync snapshot: %s", n)
		}
		if strings.HasSuffix(n, "_test.go") && judgeTestNamePattern.Match(b) {
			if changedGo[n] {
				return fmt.Errorf("mapped update is a reserved judge: %s", n)
			}
			continue
		}
		f := NamedDigest{Name: n, SHA256: Hash(b)}
		if changedGo[n] {
			updates = append(updates, f)
		} else {
			baseline = append(baseline, f)
		}
	}
	for _, n := range outputs {
		if digestNamed(updates, n) == "" {
			return fmt.Errorf("mapped Go target is not a tracked update file: %s", n)
		}
	}
	temp := out + ".preparing"
	if _, e = os.Lstat(temp); !os.IsNotExist(e) {
		return fmt.Errorf("interrupted sync draft exists: %s", temp)
	}
	if e = os.MkdirAll(temp, 0755); e != nil {
		return e
	}
	defer os.RemoveAll(temp)
	if e = AtomicJSON(filepath.Join(temp, "analysis.json"), analysis); e != nil {
		return e
	}
	raw, e := os.ReadFile(filepath.Join(temp, "analysis.json"))
	if e != nil {
		return e
	}
	rel, e := filepath.Rel(out, project)
	if e != nil {
		return e
	}
	sourceRel, e := filepath.Rel(project, snapshot)
	if e != nil {
		return e
	}
	p := modulePlan{Version: 2, Source: filepath.ToSlash(sourceRel), Revision: report.After, AnalysisSHA256: Hash(raw), Modules: []moduleDef{{ID: id, DependsOn: []string{}, Batches: []string{"changes"}}}, Batches: []batchDef{{ID: "changes", Module: id, DependsOn: []string{}, Sources: sources, References: []string{}, Outputs: outputs, Behaviors: []string{"Review the recorded upstream differences"}, Acceptance: []string{"Existing regressions plus new independent behavior tests for changed source"}}}}
	w := moduleWorkflow{Version: 2, Project: filepath.ToSlash(rel), Runs: ".portsmith/sync/" + id + "/runs", Journal: ".portsmith/sync/" + id + "/modules.json", StartPolicy: "all-prepared", Bootstrap: []string{filepath.ToSlash(mustRelative(project, out))}, Batches: map[string]batchConfig{"changes": {Status: "planned", Reason: "Changed behavior contract, independent judges, output ownership and candidate self-tests must be reviewed", Steps: []stepDef{}}}}
	if len(baseline) > 0 {
		w.Baseline = &baselineConfig{Commit: head, Files: baseline}
	}
	if len(updates) > 0 {
		w.Updates = &baselineConfig{Commit: head, Files: updates}
	}
	for name, value := range map[string]any{"plan.json": p, "workflow.json": w, "sync-report.json": report, "new-mappings.json": []SyncMapping{}} {
		if e = AtomicJSON(filepath.Join(temp, name), value); e != nil {
			return e
		}
	}
	if e = os.WriteFile(filepath.Join(temp, "go.mod"), []byte("module example.com/portsmith-sync-material\n\ngo 1.24\n"), 0644); e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(temp, "RULEBOOK.md"), []byte(DEFAULT_RULES+"\nPreserve existing Go APIs where possible. Update only files explicitly authorized by the frozen updates manifest. Treat deleted/renamed/new TS sources as planning decisions. Freeze new independent tests before calling the implementation agent.\n"), 0644); e != nil {
		return e
	}
	guide := "# Incremental migration review\n\nThis draft contains actual old/new source and a frozen target baseline. No implementation model has run.\n\nReview sync-report.json and source ownership. Add new sources to the mapping or explicitly document exclusions. Add candidate tests to outputs, a behavior contract and frozen independent judges. Fill workflow.batches.changes.steps, mark it ready, then run migrate --check and migrate --commit. Do not reuse historical executor journals.\n\nBaseline files remain read-only. Only updates.files are existing writable files; manifests, independent judges and historical receipts are immutable. Automatic Go file deletion and symbol rename decisions are not inferred from TS file changes.\n"
	if e = os.WriteFile(filepath.Join(temp, "README.md"), []byte(guide), 0644); e != nil {
		return e
	}
	return os.Rename(temp, out)
}
func mustRelative(root, name string) string {
	p, e := filepath.Rel(root, name)
	if e != nil {
		return "invalid"
	}
	return p
}

// Advance metadata only after every module in the reviewed increment is
// committed. A partial/budget-limited run must retain the old upstream baseline.
func advanceSync(ctx context.Context, o *cliOptions, r SyncReport) error {
	i, e := inspectModules(r.Plan)
	if e != nil {
		return e
	}
	if i.Plan.Revision != r.After {
		return fmt.Errorf("sync plan revision differs from requested upstream")
	}
	journal := i.Workflow.Journal
	if journal == "" {
		journal = ".portsmith/modules.json"
	}
	state, e := readJSON[moduleState](i.Project, journal)
	if e != nil {
		return e
	}
	if e = validDone(ctx, i, &state); e != nil {
		return e
	}
	if state.Pending != nil || len(state.Modules) != len(i.Plan.Modules) {
		return nil
	}
	config := o.config
	if config == "" {
		config = "migration/sync.json"
	}
	if !filepath.IsAbs(config) {
		config = filepath.Join(i.Project, config)
	}
	relative, e := filepath.Rel(i.Project, config)
	if e != nil {
		return e
	}
	relative = filepath.ToSlash(relative)
	if _, e = RelativeName(relative); e != nil {
		return e
	}
	if !strings.HasPrefix(relative, "migration/") {
		return fmt.Errorf("committed sync config must be under target migration/")
	}
	return WithLock(ctx, filepath.Join(i.Project, ".portsmith"), func() error {
		dirty, e := dirtyFiles(ctx, i.Project)
		if e != nil {
			return e
		}
		if len(dirty) != 0 {
			return fmt.Errorf("resolve working-tree changes before advancing sync baseline")
		}
		b, e := readCheckedBytes(i.Project, relative)
		if e != nil {
			return e
		}
		if Hash(b) != r.ConfigSHA256 {
			return fmt.Errorf("sync config changed during migration; baseline not advanced")
		}
		var c SyncConfig
		if e = json.Unmarshal(b, &c); e != nil {
			return e
		}
		if c.Revision != r.Before {
			return fmt.Errorf("sync baseline changed during migration")
		}
		newMappings, e := reviewedSyncMappings(i, r.Plan)
		if e != nil {
			return e
		}
		owned := map[string]int{}
		for index, m := range c.Mappings {
			owned[m.Source] = index
		}
		for _, m := range newMappings {
			for _, f := range m.GoFiles {
				if _, e = CheckedFile(i.Project, f); e != nil {
					return e
				}
			}
			if index, ok := owned[m.Source]; ok {
				c.Mappings[index] = m
			} else {
				owned[m.Source] = len(c.Mappings)
				c.Mappings = append(c.Mappings, m)
			}
		}
		remainingReferences := make([]string, 0, len(c.References))
		for _, source := range c.References {
			if _, mapped := owned[source]; !mapped {
				remainingReferences = append(remainingReferences, source)
			}
		}
		c.References = remainingReferences
		sort.Slice(c.Mappings, func(a, b int) bool { return c.Mappings[a].Source < c.Mappings[b].Source })
		c.Revision = r.After
		encoded, e := json.MarshalIndent(c, "", "  ")
		if e != nil {
			return e
		}
		encoded = append(encoded, '\n')
		head, e := gitRun(ctx, i.Project, "rev-parse", "HEAD")
		if e != nil {
			return e
		}
		pending := syncAdvance{Config: relative, Before: Hash(b), After: File{Name: relative, Data: encoded, SHA256: Hash(encoded)}, Head: head, Message: "chore: advance upstream sync baseline to " + r.After}
		if e = AtomicJSON(filepath.Join(i.Project, ".portsmith/sync-advance.json"), pending); e != nil {
			return e
		}
		return finishSyncAdvance(ctx, i.Project, pending)
	})
}

// Keep the metadata commit recoverable across interruption and failed Git hooks.
type syncAdvance struct {
	Config  string `json:"config"`
	Before  string `json:"before"`
	After   File   `json:"after"`
	Head    string `json:"head"`
	Message string `json:"message"`
}

func recoverSyncAdvance(ctx context.Context, project string) error {
	path := filepath.Join(project, ".portsmith/sync-advance.json")
	if _, e := os.Lstat(path); os.IsNotExist(e) {
		return nil
	} else if e != nil {
		return e
	}
	return WithLock(ctx, filepath.Join(project, ".portsmith"), func() error {
		p, e := readJSON[syncAdvance](project, ".portsmith/sync-advance.json")
		if e != nil {
			return e
		}
		return finishSyncAdvance(ctx, project, p)
	})
}
func finishSyncAdvance(ctx context.Context, project string, p syncAdvance) error {
	if _, e := RelativeName(p.Config); e != nil {
		return e
	}
	if !strings.HasPrefix(p.Config, "migration/") || p.After.Name != p.Config || Hash([]byte(p.After.Data)) != p.After.SHA256 {
		return fmt.Errorf("invalid pending sync metadata")
	}
	head, e := gitRun(ctx, project, "rev-parse", "HEAD")
	if e != nil {
		return e
	}
	b, e := readCheckedBytes(project, p.Config)
	if e != nil {
		return e
	}
	if head != p.Head {
		parent, e := gitRun(ctx, project, "rev-parse", "HEAD^")
		if e != nil {
			return e
		}
		msg, e := gitRun(ctx, project, "log", "-1", "--format=%s")
		if e != nil {
			return e
		}
		changed, e := gitRun(ctx, project, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
		if e != nil {
			return e
		}
		committed, e := gitBlob(ctx, project, head, p.Config)
		if e != nil {
			return e
		}
		dirty, e := dirtyFiles(ctx, project)
		if e != nil {
			return e
		}
		if parent != p.Head || msg != p.Message || changed != p.Config || Hash(b) != p.After.SHA256 || Hash(committed) != p.After.SHA256 || len(dirty) != 0 {
			return fmt.Errorf("pending sync metadata conflicts with target Git history")
		}
	} else {
		if Hash(b) != p.Before && Hash(b) != p.After.SHA256 {
			return fmt.Errorf("sync config modified during metadata recovery")
		}
		dirty, e := dirtyFiles(ctx, project)
		if e != nil {
			return e
		}
		for _, n := range dirty {
			if n != p.Config {
				return fmt.Errorf("unrelated working-tree change during metadata recovery: %s", n)
			}
		}
		if Hash(b) == p.Before {
			if e = replaceProjectFile(project, p.After); e != nil {
				return e
			}
		}
		if _, e = commitFiles(ctx, project, []string{p.Config}, p.Message); e != nil {
			return e
		}
	}
	return os.Remove(filepath.Join(project, ".portsmith/sync-advance.json"))
}

func materializeBefore(ctx context.Context, repo, snapshot string, r SyncReport) error {
	for _, change := range r.Changes {
		if change.Before == "" {
			continue
		}
		data, e := gitBlob(ctx, repo, r.Before, change.Source)
		if e != nil {
			return e
		}
		if Hash(data) != change.Before {
			return fmt.Errorf("recorded old source differs from Git: %s", change.Source)
		}
		name := "sync-before/" + r.Before + "/" + change.Source + ".txt"
		if _, e = RelativeName(name); e != nil {
			return e
		}
		p := filepath.Join(snapshot, filepath.FromSlash(name))
		if _, e = os.Lstat(p); e == nil {
			existing, e := readCheckedBytes(snapshot, name)
			if e != nil {
				return e
			}
			if Hash(existing) != change.Before {
				return fmt.Errorf("frozen old source cache changed: %s", change.Source)
			}
			continue
		} else if !os.IsNotExist(e) {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			return e
		}
		if e = os.WriteFile(p, data, 0644); e != nil {
			return e
		}
	}
	return nil
}

func reviewedSyncMappings(i *moduleInspection, plan string) ([]SyncMapping, error) {
	mappings, e := readJSON[[]SyncMapping](plan, "new-mappings.json")
	if e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	for _, m := range mappings {
		if _, e = RelativeName(m.Source); e != nil {
			return nil, e
		}
		if seen[m.Source] || len(m.GoFiles) == 0 {
			return nil, fmt.Errorf("invalid new mapping: %s", m.Source)
		}
		seen[m.Source] = true
		available := false
		for _, b := range i.Plan.Batches {
			if containsString(b.Sources, m.Source) {
				available = true
			}
		}
		if !available {
			return nil, fmt.Errorf("new mapping source is not in reviewed plan: %s", m.Source)
		}
		for _, f := range m.GoFiles {
			valid := false
			for _, b := range i.Plan.Batches {
				if containsString(b.Outputs, f) {
					valid = true
				}
			}
			if !valid || !strings.HasSuffix(f, ".go") {
				return nil, fmt.Errorf("new mapping target is not a reviewed Go output: %s", f)
			}
		}
	}
	return mappings, nil
}
