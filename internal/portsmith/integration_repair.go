package portsmith

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// moduleIntegrationRepair is a durable request to reopen the last step. The
// failure report is archived outside the candidate and bound to its bytes.
// Pending.Repair means rollback is in progress; State.Repair means generation
// may resume. Neither phase discards the candidate or its agent session.
type moduleIntegrationRepair struct {
	Key          string `json:"key"`
	Task         string `json:"task"`
	Report       string `json:"report"`
	ReportSHA256 string `json:"reportSha256"`
}

func validateIntegrationRepair(i *moduleInspection, s *moduleState) error {
	if s.Repair != nil && s.Pending != nil {
		return errors.New("Conflicting integration repair and pending transaction")
	}
	repair := s.Repair
	if s.Pending != nil {
		repair = s.Pending.Repair
	}
	if repair == nil {
		return nil
	}
	var item *moduleItem
	for idx := range i.Items {
		if i.Items[idx].Key == repair.Key {
			item = &i.Items[idx]
			break
		}
	}
	if item == nil || repair.Task != path.Join(i.Workflow.Runs, repair.Key) ||
		!strings.HasPrefix(repair.Report, repair.Task+"/integration-failures/") {
		return errors.New("Invalid integration repair task or report")
	}
	for _, module := range s.Modules {
		if module.ID == item.Module {
			return errors.New("Cannot repair an already committed module")
		}
	}
	if s.Pending != nil {
		if len(s.Steps) == 0 || s.Pending.Module != item.Module || s.Pending.Task != repair.Task {
			return errors.New("Integration rollback does not match the pending module")
		}
		last := s.Steps[len(s.Steps)-1]
		if last.Key != repair.Key || last.Task != repair.Task || last.Fingerprint != s.Pending.Fingerprint {
			return errors.New("Integration rollback does not match the final checkpoint")
		}
	} else {
		next := nextWork(i, s)
		if next.Next == nil || next.Next.Key != repair.Key {
			return errors.New("Integration repair does not match the next step")
		}
	}
	data, err := readCheckedBytes(i.Project, repair.Report)
	if err != nil {
		return err
	}
	if repair.ReportSHA256 == "" || Hash(data) != repair.ReportSHA256 {
		return errors.New("Integration failure report was modified")
	}
	return nil
}

func (m *v2Runner) integrationFeedback(key string) (string, error) {
	repair := m.state.Repair
	if repair == nil || repair.Key != key {
		return "", nil
	}
	if err := validateIntegrationRepair(m.inspected, &m.state); err != nil {
		return "", err
	}
	result, err := readJSON[ProcessResult](m.inspected.Project, repair.Report)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Project integration tests failed after cumulative candidate acceptance. No module commit was created; transaction files have been rolled back. Repair the existing final candidate, including earlier writable files in this module. Preserve frozen judges and dependencies. Candidate verification alone does not establish whole-project acceptance. Full failure report (read-only): %s\nIntegration diagnostics:\n%s",
		filepath.Join(m.inspected.Project, filepath.FromSlash(repair.Report)), result.Log), nil
}

func (m *v2Runner) beginIntegrationRepair(result ProcessResult) error {
	if err := m.checkCancel(); err != nil {
		return err
	}
	if err := m.unchanged(); err != nil {
		return err
	}
	pending := m.state.Pending
	if pending == nil || len(m.state.Steps) == 0 {
		return errors.New("Integration failure has no pending final checkpoint")
	}
	last := m.state.Steps[len(m.state.Steps)-1]
	if _, err := CheckedFile(m.inspected.Project, path.Join(pending.Task, "task.json")); err != nil {
		return err
	}
	reportDir := filepath.Join(m.inspected.Project, filepath.FromSlash(pending.Task), "integration-failures")
	info, err := os.Lstat(reportDir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(reportDir, 0o755); err != nil {
			return err
		}
		info, err = os.Lstat(reportDir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Integration failure directory must be a regular directory")
	}
	reportHandle, err := os.CreateTemp(reportDir, "failure-*.json")
	if err != nil {
		return err
	}
	reportFile := reportHandle.Name()
	if err := reportHandle.Close(); err != nil {
		return err
	}
	if err := AtomicJSON(reportFile, result); err != nil {
		return err
	}
	reportRel := path.Join(pending.Task, "integration-failures", filepath.Base(reportFile))
	data, err := readCheckedBytes(m.inspected.Project, reportRel)
	if err != nil {
		return err
	}
	pending.Repair = &moduleIntegrationRepair{Key: last.Key, Task: pending.Task, Report: reportRel, ReportSHA256: Hash(data)}
	// Persist the rollback phase before changing any target file. A restart can
	// finish a partially restored transaction without applying the bad candidate.
	if err := m.save(); err != nil {
		return err
	}
	m.log(fmt.Sprintf("%s project integration tests failed; reopening %s for agent repair. Report: %s", pending.Module, last.Key, reportFile))
	return m.rollbackIntegration()
}

func (m *v2Runner) integrationDirtyFiles(pending *modulePending) error {
	dirty, err := dirtyFiles(m.ctx, m.inspected.Project)
	if err != nil {
		return err
	}
	for _, name := range dirty {
		if digestNamed(pending.Files, name) == "" {
			return fmt.Errorf("Integration refuses unrelated working-tree changes: %s; state preserved", name)
		}
	}
	return nil
}

// rollbackIntegration restores only exactly recorded transaction bytes. All
// paths, the index, HEAD, staging and acceptance are checked before the first
// mutation. Mixed old/new bytes are valid after an interrupted rollback; a
// third hash, missing original, unrelated edit or changed HEAD stops recovery.
func (m *v2Runner) rollbackIntegration() error {
	pending := m.state.Pending
	if pending == nil || pending.Repair == nil {
		return errors.New("No integration rollback is pending")
	}
	if err := m.checkCancel(); err != nil {
		return err
	}
	if err := validateIntegrationRepair(m.inspected, &m.state); err != nil {
		return err
	}
	head, err := gitRun(m.ctx, m.inspected.Project, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != pending.Base {
		return errors.New("HEAD changed during integration rollback; state preserved")
	}
	staging := filepath.Join(m.inspected.Project, filepath.FromSlash(pending.Staging))
	staged, err := snapshotFiles(staging, 0, 0, nil)
	if err != nil {
		return err
	}
	if !digestsEqual(sortedDigests(entriesOf(staged)), sortedDigests(pending.Files)) {
		return errors.New("Module integration staging was modified")
	}
	root := filepath.Join(m.inspected.Project, filepath.FromSlash(pending.Task))
	verification, err := CurrentVerification(root)
	if err != nil {
		return err
	}
	if verification == nil || !verification.Current || verification.Report.Status != "behavior_verified" || verification.Report.Fingerprint != pending.Fingerprint {
		return errors.New("Verification became invalid before integration rollback")
	}
	if err := m.unchanged(); err != nil {
		return err
	}
	if err := m.integrationDirtyFiles(pending); err != nil {
		return err
	}
	names := []string{}
	for _, file := range pending.Files {
		if _, err := RelativeName(file.Name); err != nil {
			return err
		}
		if containsString(names, file.Name) {
			return errors.New("Duplicate integration transaction paths")
		}
		names = append(names, file.Name)
	}
	trackedRaw, err := gitRun(m.ctx, m.inspected.Project, append([]string{"ls-tree", "-r", "--name-only", "-z", pending.Base, "--"}, names...)...)
	if err != nil {
		return err
	}
	tracked := strings.Split(trackedRaw, "\x00")
	for _, before := range pending.Before {
		if digestNamed(pending.Files, before.Name) == "" {
			return errors.New("Original integration hash has no transaction output")
		}
	}
	for _, file := range pending.Files {
		old := digestNamed(pending.Before, file.Name)
		original := findFile(m.inspected.Updates, file.Name)
		if old != "" {
			if original == nil || original.SHA256 != old || !containsString(tracked, file.Name) {
				return fmt.Errorf("Integration rollback has no frozen original: %s", file.Name)
			}
		} else if containsString(tracked, file.Name) {
			return fmt.Errorf("Integration rollback refuses to remove a tracked original: %s", file.Name)
		}
		data, err := readCheckedBytes(m.inspected.Project, file.Name)
		if errors.Is(err, os.ErrNotExist) && old == "" {
			continue
		}
		if err != nil {
			return err
		}
		if got := Hash(data); got != file.SHA256 && (old == "" || got != old) {
			return fmt.Errorf("Integration rollback refuses to overwrite user changes: %s", file.Name)
		}
	}
	// Interrupted commits may have staged these exact files. Validate every
	// index entry first; never reset a user's unrelated or different staged edit.
	indexedRaw, err := gitRun(m.ctx, m.inspected.Project, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return err
	}
	indexed := []string{}
	for _, name := range strings.Split(indexedRaw, "\x00") {
		if name == "" {
			continue
		}
		expected := digestNamed(pending.Files, name)
		if expected == "" {
			return fmt.Errorf("Integration rollback refuses unrelated index changes: %s", name)
		}
		data, err := gitBlob(m.ctx, m.inspected.Project, "", name)
		if err != nil || Hash(data) != expected {
			return fmt.Errorf("Integration rollback refuses changed index bytes: %s", name)
		}
		entry, err := gitRun(m.ctx, m.inspected.Project, "ls-files", "--stage", "-z", "--", name)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(entry, "100644 ") || strings.Count(entry, "\x00") != 1 {
			return fmt.Errorf("Integration rollback refuses changed index mode: %s", name)
		}
		indexed = append(indexed, name)
	}
	if len(indexed) > 0 {
		if _, err := gitRun(m.ctx, m.inspected.Project, append([]string{"restore", "--staged", "--source=" + pending.Base, "--"}, indexed...)...); err != nil {
			return err
		}
	}
	for _, file := range pending.Files {
		if err := m.checkCancel(); err != nil {
			return err
		}
		old := digestNamed(pending.Before, file.Name)
		data, err := readCheckedBytes(m.inspected.Project, file.Name)
		if errors.Is(err, os.ErrNotExist) && old == "" {
			continue
		}
		if err != nil {
			return err
		}
		if old != "" && Hash(data) == old {
			continue
		}
		if Hash(data) != file.SHA256 {
			return fmt.Errorf("Integration rollback refuses to overwrite user changes: %s", file.Name)
		}
		if old != "" {
			if err := replaceProjectFile(m.inspected.Project, *findFile(m.inspected.Updates, file.Name)); err != nil {
				return err
			}
		} else {
			checked, err := CheckedFile(m.inspected.Project, file.Name)
			if err != nil {
				return err
			}
			if err := os.Remove(checked); err != nil {
				return err
			}
		}
	}
	if err := m.clean(); err != nil {
		return err
	}
	// The final candidate may now change without invalidating accepted earlier
	// checkpoints on restart. All earlier candidates and their evidence survive.
	m.state.Steps = m.state.Steps[:len(m.state.Steps)-1]
	m.state.Repair = pending.Repair
	m.state.Pending = nil
	return m.save()
}
