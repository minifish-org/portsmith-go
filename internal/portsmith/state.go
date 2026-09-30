// state.go ports src/state.ts: candidate task status, plan status and the
// acceptance receipt. AcceptTask itself lives in verify.go beside the verifier
// that produces the receipt it demands, but the status projections below are
// owned here.
package portsmith

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// acceptanceReceipt is the record written beside the task and copied into the
// exported directory. It ports the `receipt` object of src/state.ts.
type acceptanceReceipt struct {
	At           string        `json:"at"`
	Fingerprint  string        `json:"fingerprint"`
	Out          string        `json:"out"`
	Files        []NamedDigest `json:"files"`
	Verification Verification  `json:"verification"`
}

// AcceptTask exports a verified candidate to a fresh directory. It ports
// src/state.ts#acceptTask: acceptance demands a current, independent,
// behavior-verified receipt and the destination must not already exist.
func AcceptTask(rootInput, out string) (string, error) {
	root, err := filepath.EvalSymlinks(rootInput)
	if err != nil {
		return "", err
	}
	verification, err := CurrentVerification(root)
	if err != nil {
		return "", err
	}
	if verification == nil || !verification.Current || verification.Report.Status != "behavior_verified" {
		return "", errors.New("Acceptance requires independent behavior verification of the current candidate; compilation or candidate tests alone are insufficient")
	}
	files, err := CandidateFiles(root)
	if err != nil {
		return "", err
	}
	digest, err := Fingerprint(root)
	if err != nil {
		return "", err
	}
	destination, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	if err := os.Mkdir(destination, 0o777); err != nil {
		return "", err
	}
	if err := copyFiles(destination, files); err != nil {
		return "", err
	}
	receipt := acceptanceReceipt{
		At:           time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Fingerprint:  digest,
		Out:          destination,
		Files:        make([]NamedDigest, 0, len(files)),
		Verification: verification.Report,
	}
	for _, file := range files {
		receipt.Files = append(receipt.Files, NamedDigest{Name: file.Name, SHA256: file.SHA256})
	}
	if err := AtomicJSON(filepath.Join(destination, "PORTSMITH-RECEIPT.json"), receipt); err != nil {
		return "", err
	}
	if err := AtomicJSON(filepath.Join(root, "acceptance.json"), receipt); err != nil {
		return "", err
	}
	return destination, nil
}

// TaskStatus reports the observable state of a prepared task without mutating
// anything. The states mirror src/state.ts exactly: prepared, generated,
// stale_verification, the report status, accepted, export_changed.
func TaskStatus(rootInput string) (json.RawMessage, error) {
	root, err := filepath.EvalSymlinks(rootInput)
	if err != nil {
		return nil, err
	}
	_, _, task, err := loadTask(root)
	if err != nil {
		return nil, err
	}
	files, err := CandidateFiles(root)
	if err != nil {
		return nil, err
	}
	verification, err := CurrentVerification(root)
	if err != nil {
		return nil, err
	}
	state := "prepared"
	for _, file := range files {
		if strings.HasSuffix(file.Name, ".go") {
			state = "generated"
			break
		}
	}
	if verification != nil {
		if verification.Current {
			state = verification.Report.Status
		} else {
			state = "stale_verification"
		}
	}
	acceptance, err := readJSON[acceptanceReceipt](root, "acceptance.json")
	if err == nil {
		fingerprint, fingerprintErr := Fingerprint(root)
		if fingerprintErr != nil {
			return nil, fingerprintErr
		}
		if acceptance.Fingerprint == fingerprint &&
			verification != nil && verification.Current &&
			verification.Report.Status == "behavior_verified" {
			exported, snapshotErr := snapshotFiles(acceptance.Out, 0, 0, nil)
			if snapshotErr == nil {
				entries := make([]NamedDigest, 0, len(exported))
				for _, file := range exported {
					if file.Name == "PORTSMITH-RECEIPT.json" {
						continue
					}
					entries = append(entries, NamedDigest{Name: file.Name, SHA256: file.SHA256})
				}
				currentJSON, marshalErr := json.Marshal(entries)
				if marshalErr != nil {
					return nil, marshalErr
				}
				recordedJSON, marshalErr := json.Marshal(acceptance.Files)
				if marshalErr != nil {
					return nil, marshalErr
				}
				if Hash(currentJSON) == Hash(recordedJSON) {
					state = "accepted"
				} else {
					state = "export_changed"
				}
			} else if !errors.Is(snapshotErr, os.ErrNotExist) {
				return nil, snapshotErr
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	document := struct {
		Unit         string `json:"unit"`
		State        string `json:"state"`
		Revision     string `json:"revision"`
		Files        int    `json:"files"`
		Verification bool   `json:"verification"`
	}{
		Unit:         task.Unit,
		State:        state,
		Revision:     task.Revision,
		Files:        len(files),
		Verification: verification != nil && verification.Current,
	}
	return json.Marshal(document)
}

type planStatusEntry struct {
	Unit      string   `json:"unit"`
	State     string   `json:"state"`
	BlockedBy []string `json:"blockedBy"`
}

// PlanStatus reports one state per planned unit in plan order. It ports
// `planStatus(root, runs)` from src/state.ts, including the dependents-blocked
// projection.
func PlanStatus(rootInput, runsInput string) (json.RawMessage, error) {
	root, err := filepath.EvalSymlinks(rootInput)
	if err != nil {
		return nil, err
	}
	plan, err := loadPlan(root)
	if err != nil {
		return nil, err
	}
	digest, err := planDigest(root)
	if err != nil {
		return nil, err
	}
	statuses := make(map[string]string, len(plan.Units))
	for _, unit := range plan.Units {
		taskRoot := filepath.Join(runsInput, unit.ID)
		status, statusErr := TaskStatus(taskRoot)
		if statusErr != nil {
			if errors.Is(statusErr, os.ErrNotExist) {
				statuses[unit.ID] = "planned"
				continue
			}
			statuses[unit.ID] = "invalid: " + statusErr.Error()
			continue
		}
		var observed struct {
			Unit  string `json:"unit"`
			State string `json:"state"`
		}
		if unmarshalErr := json.Unmarshal(status, &observed); unmarshalErr != nil {
			statuses[unit.ID] = "invalid: " + unmarshalErr.Error()
			continue
		}
		if observed.Unit != unit.ID {
			statuses[unit.ID] = "invalid: Task directory does not match unit: " + unit.ID
			continue
		}
		_, _, task, loadErr := loadTask(taskRoot)
		if loadErr != nil {
			statuses[unit.ID] = "invalid: " + loadErr.Error()
			continue
		}
		if task.PlanDigest == digest {
			statuses[unit.ID] = observed.State
		} else {
			statuses[unit.ID] = "plan_changed"
		}
	}
	result := make([]planStatusEntry, 0, len(plan.Units))
	for _, unit := range plan.Units {
		blocked := []string{}
		for _, dependency := range unit.DependsOn {
			state := statuses[dependency]
			if state != "behavior_verified" && state != "accepted" {
				blocked = append(blocked, dependency)
			}
		}
		result = append(result, planStatusEntry{
			Unit:      unit.ID,
			State:     statuses[unit.ID],
			BlockedBy: blocked,
		})
	}
	return json.Marshal(result)
}
