package portsmith

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

func findFile(files []File, name string) *File {
	for i := range files {
		if files[i].Name == name {
			return &files[i]
		}
	}
	return nil
}
func fileNamed(files []File, name string) bool { return findFile(files, name) != nil }
func digestNamed(files []NamedDigest, name string) string {
	for _, f := range files {
		if f.Name == name {
			return f.SHA256
		}
	}
	return ""
}
func committedFile(state moduleState, name string) bool {
	for _, m := range state.Modules {
		if digestNamed(m.Files, name) != "" {
			return true
		}
	}
	return false
}

// Git supplies the original bytes even after a transaction has partially
// applied its replacements. Working-tree bytes are never used as a new baseline.
func gitBlob(ctx context.Context, root, revision, name string) ([]byte, error) {
	c := exec.CommandContext(ctx, "git", "cat-file", "blob", revision+":"+name)
	c.Dir = root
	c.Env = envList(CleanEnv(false, false))
	b, e := c.Output()
	if e != nil {
		return nil, fmt.Errorf("read frozen Git blob %s: %w", name, e)
	}
	return b, nil
}

func inspectUpdates(project string, w moduleWorkflow, owners map[string]string, judges map[string]bool, baseline []File) ([]File, error) {
	if w.Updates == nil {
		return nil, nil
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(w.Updates.Commit) || len(w.Updates.Files) == 0 {
		return nil, fmt.Errorf("updates requires a full commit and non-empty file manifest")
	}
	if w.Journal == "" || w.Journal == ".portsmith/modules.json" {
		return nil, fmt.Errorf("updates requires a separate journal")
	}
	if w.Baseline != nil && w.Baseline.Commit != w.Updates.Commit {
		return nil, fmt.Errorf("baseline and updates must use the same commit")
	}
	if _, e := gitRun(context.Background(), project, "merge-base", "--is-ancestor", w.Updates.Commit, "HEAD"); e != nil {
		return nil, e
	}
	files := []File{}
	seen := map[string]bool{}
	for _, f := range w.Updates.Files {
		if e := outputName(f.Name); e != nil {
			return nil, e
		}
		if !regexp.MustCompile(`^(packages|internal|cmd|docs|examples)/`).MatchString(f.Name) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(f.SHA256) || seen[f.Name] || owners[f.Name] == "" || judges[f.Name] || fileNamed(baseline, f.Name) {
			return nil, fmt.Errorf("invalid or overlapping update target: %s", f.Name)
		}
		seen[f.Name] = true
		tree, e := gitRun(context.Background(), project, "ls-tree", w.Updates.Commit, "--", f.Name)
		if e != nil {
			return nil, e
		}
		if !strings.HasPrefix(tree, "100644 blob ") {
			return nil, fmt.Errorf("update target is not a regular tracked file: %s", f.Name)
		}
		b, e := gitBlob(context.Background(), project, w.Updates.Commit, f.Name)
		if e != nil {
			return nil, e
		}
		if Hash(b) != f.SHA256 {
			return nil, fmt.Errorf("frozen update hash mismatch: %s", f.Name)
		}
		if strings.HasSuffix(f.Name, "_test.go") && judgeTestNamePattern.Match(b) {
			return nil, fmt.Errorf("update target contains reserved independent judges: %s", f.Name)
		}
		files = append(files, File{Name: f.Name, Data: b, SHA256: Hash(b)})
	}
	return files, nil
}

func validateUpdateWorkspace(i *moduleInspection, s *moduleState) error {
	for _, f := range i.Updates {
		expected := f.SHA256
		for _, m := range s.Modules {
			if h := digestNamed(m.Files, f.Name); h != "" {
				expected = h
			}
		}
		b, e := readCheckedBytes(i.Project, f.Name)
		if e != nil {
			return e
		}
		got := Hash(b)
		if got == expected {
			continue
		}
		if s.Pending != nil && digestNamed(s.Pending.Before, f.Name) == f.SHA256 && digestNamed(s.Pending.Files, f.Name) == got {
			continue
		}
		return fmt.Errorf("Update target changed outside the migration: %s", f.Name)
	}
	// Refuse an undeclared replacement before spending any model calls.
	for _, item := range i.Items {
		names := append([]string{}, item.Spec.Outputs...)
		for _, asset := range item.Assets {
			names = append(names, asset.Name)
		}
		for _, judge := range item.Judge {
			names = append(names, judge.Name)
		}
		for _, name := range names {
			exists, err := fileExists(filepath.Join(i.Project, filepath.FromSlash(name)))
			if err != nil {
				return err
			}
			if !exists || fileNamed(i.Updates, name) || committedFile(*s, name) {
				continue
			}
			if s.Pending != nil && digestNamed(s.Pending.Files, name) != "" {
				continue
			}
			return fmt.Errorf("Integration refuses to overwrite an existing file: %s; declare its frozen hash in updates", name)
		}
	}
	return nil
}

func replaceProjectFile(root string, f File) error {
	destination, e := CheckedFile(root, f.Name)
	if e != nil {
		return e
	}
	info, e := os.Stat(destination)
	if e != nil {
		return e
	}
	temp, e := os.CreateTemp(filepath.Dir(destination), ".portsmith-replace-")
	if e != nil {
		return e
	}
	name := temp.Name()
	defer os.Remove(name)
	if e = temp.Chmod(info.Mode().Perm()); e == nil {
		_, e = temp.Write(f.Data)
	}
	if e == nil {
		e = temp.Sync()
	}
	closeErr := temp.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, destination)
}
