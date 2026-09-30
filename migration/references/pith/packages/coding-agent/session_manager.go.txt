// Session storage for the embedded SDK.
//
// This file ports the storage half of
// packages/coding-agent/src/core/session-manager.ts and session-cwd.ts from Pi
// at revision f07218c4d4bbc12bef056a7058c3dd49dfe41abe.
//
// The on-disk format is an append-only JSONL file:
//
//	{"type":"session","version":3,"id":"...","timestamp":"...","cwd":"..."}
//	{"id":"...","parentId":"...","type":"message","payload":{...}}
//	{"id":"...","parentId":"...","type":"custom","payload":{...}}
//
// Each non-header record is the frozen public SessionEntry shape, so unknown
// payload fields survive a load/save round trip verbatim. Branch moves the leaf
// and appends a small internal "__leaf__" control record; that record never
// appears in Entries()/Context() but makes the leaf pointer durable without a
// subsequent append. A memory-only manager (empty path) keeps the same model
// without touching the filesystem.
//
// Supported file version: 3 (CurrentSessionVersion). The SDK deliberately does
// not read historical Pi v1/v2 JSONL that stored type-specific fields inline;
// such files are rejected with an explicit error instead of being silently
// reinterpreted. See NOTES.md for the crash-tail recovery policy.
//
// Upstream: Copyright (c) 2025 Mario Zechner, MIT License.
package codingagent

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	harnessmessages "github.com/minifish-org/pith/packages/agent/harness/messages"
	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	aitypes "github.com/minifish-org/pith/packages/ai/types"
	aiutils "github.com/minifish-org/pith/packages/ai/utils"
)

// CurrentSessionVersion is the session file version written and accepted by
// this SDK. Files with any other explicit version are rejected.
const CurrentSessionVersion = 3

// leafRecordType is the internal control record that persists the active leaf
// after a Branch without creating a user-visible session entry.
const leafRecordType = "__leaf__"

// ErrSessionClosed is returned by append/branch operations after Close.
var ErrSessionClosed = errors.New("session manager is closed")

// SessionHeader is the first JSONL record of a persisted session.
type SessionHeader struct {
	Type          string  `json:"type"`
	Version       *int    `json:"version,omitempty"`
	ID            string  `json:"id"`
	Timestamp     string  `json:"timestamp"`
	Cwd           string  `json:"cwd"`
	ParentSession *string `json:"parentSession,omitempty"`
}

// NewSessionOptions configures a newly created session.
type NewSessionOptions struct {
	ID            *string
	ParentSession *string
}

// SessionEntryBase carries the identity shared by every typed session entry.
type SessionEntryBase struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	ParentID  *string `json:"parentId"`
	Timestamp string  `json:"timestamp"`
}

// SessionMessageEntry is the typed shape of a "message" entry.
type SessionMessageEntry struct {
	SessionEntryBase
	Message json.RawMessage `json:"message"`
}

// ThinkingLevelChangeEntry records a thinking-level change.
type ThinkingLevelChangeEntry struct {
	SessionEntryBase
	ThinkingLevel string `json:"thinkingLevel"`
}

// ModelChangeEntry records a provider/model change.
type ModelChangeEntry struct {
	SessionEntryBase
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// UsageEntry records model-attributed usage that does not enter LLM context.
type UsageEntry struct {
	SessionEntryBase
	Kind     string        `json:"kind"`
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	Usage    aitypes.Usage `json:"usage"`
	Note     *string       `json:"note,omitempty"`
}

// CompactionEntry is the typed shape of a compaction checkpoint.
type CompactionEntry struct {
	SessionEntryBase
	Summary          string          `json:"summary"`
	FirstKeptEntryID string          `json:"firstKeptEntryId"`
	TokensBefore     float64         `json:"tokensBefore"`
	Details          json.RawMessage `json:"details,omitempty"`
	Usage            *aitypes.Usage  `json:"usage,omitempty"`
	FromHook         *bool           `json:"fromHook,omitempty"`
	SystemMessage    json.RawMessage `json:"systemMessage,omitempty"`
}

// BranchSummaryEntry summarizes a branch the conversation returned from.
type BranchSummaryEntry struct {
	SessionEntryBase
	FromID   string          `json:"fromId"`
	Summary  string          `json:"summary"`
	Details  json.RawMessage `json:"details,omitempty"`
	Usage    *aitypes.Usage  `json:"usage,omitempty"`
	FromHook *bool           `json:"fromHook,omitempty"`
}

// CustomEntry stores extension state that never enters LLM context.
type CustomEntry struct {
	SessionEntryBase
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// LabelEntry is a user-defined bookmark on another entry.
type LabelEntry struct {
	SessionEntryBase
	TargetID string  `json:"targetId"`
	Label    *string `json:"label"`
}

// SessionInfoEntry carries session metadata such as a display name.
type SessionInfoEntry struct {
	SessionEntryBase
	Name *string `json:"name,omitempty"`
}

// CustomMessageEntry injects a custom message into LLM context.
type CustomMessageEntry struct {
	SessionEntryBase
	CustomType string          `json:"customType"`
	Content    json.RawMessage `json:"content"`
	Details    json.RawMessage `json:"details,omitempty"`
	Display    bool            `json:"display"`
}

// ContextEditableContent is the JSON content union a context edit may replace.
type ContextEditableContent = json.RawMessage

// ContextEditReplacement is the replacement body of a context edit. A nil
// Content means the target is omitted from model context.
type ContextEditReplacement struct {
	Content json.RawMessage `json:"content"`
}

// ContextEditEntry is an append-only change to an earlier entry's context.
type ContextEditEntry struct {
	SessionEntryBase
	TargetID    string                  `json:"targetId"`
	Replacement *ContextEditReplacement `json:"replacement"`
}

// SessionEntry is the canonical serialized entry: a unique ID, an optional
// parent, a type discriminator and a raw payload. Payload keeps unknown fields
// verbatim.
type SessionEntry struct {
	ID       string          `json:"id"`
	ParentID string          `json:"parentId,omitempty"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
}

// FileEntry is one parsed JSONL record: exactly one of Header or Entry is set.
type FileEntry struct {
	Header *SessionHeader
	Entry  *SessionEntry
}

// SessionTreeNode is a defensive tree view of the session.
type SessionTreeNode struct {
	Entry          SessionEntry       `json:"entry"`
	Children       []*SessionTreeNode `json:"children"`
	Label          *string            `json:"label,omitempty"`
	LabelTimestamp *string            `json:"labelTimestamp,omitempty"`
}

// ProjectedSessionEntry pairs one selected entry with its model messages.
type ProjectedSessionEntry struct {
	SourceEntry SessionEntry
	Messages    []agenttypes.AgentMessage
}

// SessionModelRef identifies the provider/model active at a point in a session.
type SessionModelRef struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// SessionProjection is the provenance-preserving, compaction-aware view.
type SessionProjection struct {
	Entries       []ProjectedSessionEntry
	Messages      []agenttypes.AgentMessage
	ThinkingLevel string
	Model         *SessionModelRef
}

// SessionContext is the finalized model context.
type SessionContext struct {
	Messages      []agenttypes.AgentMessage
	ThinkingLevel string
	Model         *SessionModelRef
}

// SessionInfo is discovery metadata for one session file.
type SessionInfo struct {
	Path              string
	ID                string
	Cwd               string
	Name              *string
	ParentSessionPath *string
	Created           time.Time
	Modified          time.Time
	MessageCount      int
	FirstMessage      string
	AllMessagesText   string
}

// SessionListProgress reports session discovery progress.
type SessionListProgress func(loaded, total int, partial []SessionInfo)

// SessionCwdSource is the minimal read surface used by the cwd checks.
type SessionCwdSource interface {
	GetCwd() string
	SessionFile() string
}

// ReadonlySessionManager is the read-only session surface.
type ReadonlySessionManager interface {
	GetCwd() string
	SessionFile() string
	SessionID() string
	LeafID() string
	GetLeafEntry() (SessionEntry, bool)
	GetEntry(id string) (SessionEntry, bool)
	GetLabel(id string) (string, bool)
	GetBranch(fromID string) []SessionEntry
	BuildContextEntries() []SessionEntry
	BuildSessionProjection() SessionProjection
	BuildSessionContext() SessionContext
	Header() SessionHeader
	Entries() []SessionEntry
	GetTree() []SessionTreeNode
}

// SessionCwdIssue describes a stored session cwd that no longer exists.
type SessionCwdIssue struct {
	SessionFile *string `json:"sessionFile,omitempty"`
	SessionCwd  string  `json:"sessionCwd"`
	FallbackCwd string  `json:"fallbackCwd"`
}

// MissingSessionCwdError is returned by AssertSessionCwdExists.
type MissingSessionCwdError struct {
	Issue SessionCwdIssue
}

// Error implements the error interface.
func (e *MissingSessionCwdError) Error() string {
	return FormatMissingSessionCwdError(e.Issue)
}

// SessionManager manages an append-only session tree.
//
// A memory-only manager is created by opening the empty path. All mutation is
// serialized; Entries/Context/GetBranch return defensive copies.
type SessionManager struct {
	mu      sync.Mutex
	persist bool
	file    string
	cwd     string
	header  SessionHeader
	entries []SessionEntry
	byID    map[string]int
	leaf    string
	labels  map[string]string
	closed  bool
}

// ---------------------------------------------------------------------------
// Public construction and primitives
// ---------------------------------------------------------------------------

// AssertValidSessionID validates an explicit session id: non-empty, only
// alphanumerics, '-', '_' and '.', and it must start and end with an
// alphanumeric character.
func AssertValidSessionID(id string) error {
	if id == "" {
		return errors.New("session id must not be empty")
	}
	for index, r := range id {
		alphanumeric := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if index == 0 || index == len(id)-1 {
			if !alphanumeric {
				return fmt.Errorf("session id must start and end with an alphanumeric character: %q", id)
			}
			continue
		}
		if !alphanumeric && r != '-' && r != '_' && r != '.' {
			return fmt.Errorf("session id contains an invalid character: %q", id)
		}
	}
	return nil
}

// OpenSession opens or creates a session. An empty file creates a memory-only
// manager. A non-empty path is durable and uses mode 0600.
func OpenSession(file string) (*SessionManager, error) {
	manager := &SessionManager{
		persist: file != "",
		byID:    map[string]int{},
		labels:  map[string]string{},
	}
	if file == "" {
		manager.initNewSession(nil)
		return manager, nil
	}
	manager.file = filepath.Clean(file)

	info, err := os.Stat(manager.file)
	switch {
	case errors.Is(err, os.ErrNotExist):
		manager.initNewSession(nil)
		if err := manager.createFile(); err != nil {
			return nil, err
		}
		return manager, nil
	case err != nil:
		return nil, err
	case info.IsDir():
		return nil, fmt.Errorf("session path is a directory: %s", manager.file)
	case info.Size() == 0:
		manager.initNewSession(nil)
		if err := manager.createFile(); err != nil {
			return nil, err
		}
		return manager, nil
	}

	data, err := os.ReadFile(manager.file)
	if err != nil {
		return nil, err
	}
	records, meta, err := parseRecords(data, true)
	if err != nil {
		return nil, err
	}
	if err := manager.adopt(records); err != nil {
		return nil, err
	}
	if meta.recoveredTail {
		if err := os.Truncate(manager.file, int64(meta.validPrefix)); err != nil {
			return nil, err
		}
	}
	if meta.needsTermination {
		if err := manager.terminateFile(); err != nil {
			return nil, err
		}
	}
	return manager, nil
}

// Append appends a new entry of kind under the current leaf and advances the
// leaf. It never mutates in-memory state if the durable write fails.
func (s *SessionManager) Append(kind string, payload json.RawMessage) (SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return SessionEntry{}, ErrSessionClosed
	}
	if strings.TrimSpace(kind) == "" {
		return SessionEntry{}, errors.New("session entry type must not be empty")
	}
	if kind == leafRecordType {
		return SessionEntry{}, fmt.Errorf("session entry type %q is reserved", kind)
	}
	stored, err := normalizePayload(payload)
	if err != nil {
		return SessionEntry{}, err
	}
	id, err := s.nextID()
	if err != nil {
		return SessionEntry{}, err
	}
	entry := SessionEntry{
		ID:       id,
		ParentID: s.leaf,
		Type:     kind,
		Payload:  stored,
	}
	if s.persist {
		if err := s.appendRaw(entry); err != nil {
			return SessionEntry{}, err
		}
	}
	s.entries = append(s.entries, entry)
	s.byID[entry.ID] = len(s.entries) - 1
	s.leaf = entry.ID
	return cloneSessionEntry(entry), nil
}

// Entries lists every session entry in append order, excluding the header and
// internal control records. The returned slice is an immutable snapshot.
func (s *SessionManager) Entries() []SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSessionEntries(s.entries)
}

// Context lists the active root-to-leaf branch. The returned slice is an
// immutable snapshot.
func (s *SessionManager) Context() []SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSessionEntries(s.branchPathLocked(s.leaf))
}

// Branch moves the durable leaf pointer to id without deleting siblings. An
// empty id branches to the root (before any entry).
func (s *SessionManager) Branch(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionClosed
	}
	if id != "" {
		if _, ok := s.byID[id]; !ok {
			return fmt.Errorf("session entry %q not found", id)
		}
	}
	if s.persist {
		if err := s.persistLeaf(id); err != nil {
			return err
		}
	}
	s.leaf = id
	return nil
}

// LeafID returns the current leaf id, or the empty string for the root.
func (s *SessionManager) LeafID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaf
}

// Close releases the session. It is idempotent; appends after Close fail.
func (s *SessionManager) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// ---------------------------------------------------------------------------
// Typed append helpers
// ---------------------------------------------------------------------------

// AppendMessage appends a provider/agent message (raw JSON).
func (s *SessionManager) AppendMessage(message json.RawMessage) (SessionEntry, error) {
	return s.Append("message", message)
}

// AppendThinkingLevelChange appends a thinking-level change.
func (s *SessionManager) AppendThinkingLevelChange(level string) (SessionEntry, error) {
	payload, err := json.Marshal(map[string]any{"thinkingLevel": level})
	if err != nil {
		return SessionEntry{}, err
	}
	return s.Append("thinking_level_change", payload)
}

// AppendModelChange appends a provider/model change.
func (s *SessionManager) AppendModelChange(provider, modelID string) (SessionEntry, error) {
	payload, err := json.Marshal(map[string]any{"provider": provider, "modelId": modelID})
	if err != nil {
		return SessionEntry{}, err
	}
	return s.Append("model_change", payload)
}

// AppendUsage appends model-attributed usage.
func (s *SessionManager) AppendUsage(kind, provider, model string, usage aitypes.Usage, note *string) (SessionEntry, error) {
	payload := map[string]any{"kind": kind, "provider": provider, "model": model, "usage": usage}
	if note != nil {
		payload["note"] = *note
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return SessionEntry{}, err
	}
	return s.Append("usage", data)
}

// CompactionInput configures AppendCompaction. Go has no optional positional
// arguments, so the upstream parameters are grouped here.
type CompactionInput struct {
	Summary          string
	FirstKeptEntryID string
	TokensBefore     float64
	Details          json.RawMessage
	FromHook         *bool
	Usage            *aitypes.Usage
	SystemMessage    json.RawMessage
}

// AppendCompaction appends a compaction checkpoint.
func (s *SessionManager) AppendCompaction(input CompactionInput) (SessionEntry, error) {
	payload := map[string]any{
		"summary":          input.Summary,
		"firstKeptEntryId": input.FirstKeptEntryID,
		"tokensBefore":     input.TokensBefore,
	}
	if len(input.Details) > 0 {
		payload["details"] = input.Details
	}
	if input.FromHook != nil {
		payload["fromHook"] = *input.FromHook
	}
	if input.Usage != nil {
		payload["usage"] = input.Usage
	}
	if len(input.SystemMessage) > 0 {
		payload["systemMessage"] = input.SystemMessage
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return SessionEntry{}, err
	}
	return s.Append("compaction", data)
}

// AppendCustomEntry appends extension state that does not enter LLM context.
func (s *SessionManager) AppendCustomEntry(customType string, data json.RawMessage) (SessionEntry, error) {
	payload := map[string]any{"customType": customType}
	if len(data) > 0 {
		payload["data"] = data
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return SessionEntry{}, err
	}
	return s.Append("custom", encoded)
}

// AppendSessionInfo appends a display-name record. Newlines are collapsed.
func (s *SessionManager) AppendSessionInfo(name string) (SessionEntry, error) {
	cleaned := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(name))
	payload, err := json.Marshal(map[string]any{"name": cleaned})
	if err != nil {
		return SessionEntry{}, err
	}
	return s.Append("session_info", payload)
}

// AppendCustomMessageEntry appends a custom message that enters LLM context.
func (s *SessionManager) AppendCustomMessageEntry(
	customType string,
	content json.RawMessage,
	display bool,
	details json.RawMessage,
) (SessionEntry, error) {
	payload := map[string]any{"customType": customType, "content": content, "display": display}
	if len(details) > 0 {
		payload["details"] = details
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return SessionEntry{}, err
	}
	return s.Append("custom_message", data)
}

// AppendContextEdit appends a branch-local edit to an earlier entry.
func (s *SessionManager) AppendContextEdit(targetID string, replacement json.RawMessage) (SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return SessionEntry{}, ErrSessionClosed
	}
	if targetID == "" {
		return SessionEntry{}, errors.New("context edit target id must not be empty")
	}
	if _, ok := s.byID[targetID]; !ok {
		return SessionEntry{}, fmt.Errorf("session entry %q not found", targetID)
	}
	onBranch := false
	for _, entry := range s.branchPathLocked(s.leaf) {
		if entry.ID == targetID {
			onBranch = true
			break
		}
	}
	if !onBranch {
		return SessionEntry{}, fmt.Errorf("session entry %q is not on the active branch", targetID)
	}
	stored, err := normalizePayload(replacement)
	if err != nil {
		return SessionEntry{}, err
	}
	payload, err := json.Marshal(map[string]any{"targetId": targetID, "replacement": stored})
	if err != nil {
		return SessionEntry{}, err
	}
	// Append re-acquires the lock; call the unlocked core directly.
	return s.appendLocked("context_edit", payload)
}

// AppendLabelChange sets or clears a label on targetID.
func (s *SessionManager) AppendLabelChange(targetID string, label *string) (SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return SessionEntry{}, ErrSessionClosed
	}
	if _, ok := s.byID[targetID]; !ok {
		return SessionEntry{}, fmt.Errorf("session entry %q not found", targetID)
	}
	payload, err := json.Marshal(map[string]any{"targetId": targetID, "label": label})
	if err != nil {
		return SessionEntry{}, err
	}
	entry, err := s.appendLocked("label", payload)
	if err != nil {
		return SessionEntry{}, err
	}
	if label != nil {
		s.labels[targetID] = *label
	} else {
		delete(s.labels, targetID)
	}
	return entry, nil
}

// BranchWithSummary moves the leaf and appends a branch_summary entry.
func (s *SessionManager) BranchWithSummary(
	branchFromID string,
	summary string,
	details json.RawMessage,
	fromHook *bool,
	usage *aitypes.Usage,
) (SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return SessionEntry{}, ErrSessionClosed
	}
	if branchFromID != "" {
		if _, ok := s.byID[branchFromID]; !ok {
			return SessionEntry{}, fmt.Errorf("session entry %q not found", branchFromID)
		}
	}
	fromID := s.leaf
	if fromID == "" {
		fromID = "root"
	}
	payload := map[string]any{"fromId": fromID, "summary": summary}
	if len(details) > 0 {
		payload["details"] = details
	}
	if fromHook != nil {
		payload["fromHook"] = *fromHook
	}
	if usage != nil {
		payload["usage"] = usage
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return SessionEntry{}, err
	}
	// Branching first; if the leaf write fails nothing is mutated.
	if s.persist {
		if err := s.persistLeaf(branchFromID); err != nil {
			return SessionEntry{}, err
		}
	}
	s.leaf = branchFromID
	return s.appendLocked("branch_summary", data)
}

// ResetLeaf branches to the root (before any entry).
func (s *SessionManager) ResetLeaf() error {
	return s.Branch("")
}

// ---------------------------------------------------------------------------
// Read helpers
// ---------------------------------------------------------------------------

// SessionID returns the session id from the header.
func (s *SessionManager) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header.ID
}

// SessionFile returns the backing file, or "" in memory-only mode.
func (s *SessionManager) SessionFile() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file
}

// IsPersisted reports whether the session writes to disk.
func (s *SessionManager) IsPersisted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persist
}

// GetCwd returns the stored working directory.
func (s *SessionManager) GetCwd() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

// Header returns a copy of the session header.
func (s *SessionManager) Header() SessionHeader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header
}

// GetEntry returns one entry by id.
func (s *SessionManager) GetEntry(id string) (SessionEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.byID[id]
	if !ok {
		return SessionEntry{}, false
	}
	return cloneSessionEntry(s.entries[index]), true
}

// GetLeafEntry returns the current leaf entry.
func (s *SessionManager) GetLeafEntry() (SessionEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.byID[s.leaf]
	if !ok {
		return SessionEntry{}, false
	}
	return cloneSessionEntry(s.entries[index]), true
}

// GetLabel returns the resolved label for an entry.
func (s *SessionManager) GetLabel(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	label, ok := s.labels[id]
	return label, ok
}

// GetBranch walks from fromID (or the current leaf when empty) to the root.
func (s *SessionManager) GetBranch(fromID string) []SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fromID == "" {
		return cloneSessionEntries(s.branchPathLocked(s.leaf))
	}
	if _, ok := s.byID[fromID]; !ok {
		return []SessionEntry{}
	}
	return cloneSessionEntries(s.branchPathLocked(fromID))
}

// BuildContextEntries returns the compaction-aware active entry list.
func (s *SessionManager) BuildContextEntries() []SessionEntry {
	s.mu.Lock()
	entries := cloneSessionEntries(s.entries)
	leaf := s.leaf
	s.mu.Unlock()
	return BuildContextEntries(entries, &leaf)
}

// BuildSessionProjection returns the provenance-preserving context view.
func (s *SessionManager) BuildSessionProjection() SessionProjection {
	s.mu.Lock()
	entries := cloneSessionEntries(s.entries)
	leaf := s.leaf
	s.mu.Unlock()
	return BuildSessionProjection(entries, &leaf)
}

// BuildSessionContext returns the finalized model context.
func (s *SessionManager) BuildSessionContext() SessionContext {
	projection := s.BuildSessionProjection()
	return SessionContext{
		Messages:      projection.Messages,
		ThinkingLevel: projection.ThinkingLevel,
		Model:         projection.Model,
	}
}

// GetTree returns a defensive tree view of all entries.
func (s *SessionManager) GetTree() []SessionTreeNode {
	s.mu.Lock()
	entries := cloneSessionEntries(s.entries)
	labels := make(map[string]string, len(s.labels))
	for key, value := range s.labels {
		labels[key] = value
	}
	s.mu.Unlock()

	nodes := make(map[string]*SessionTreeNode, len(entries))
	var roots []*SessionTreeNode
	for i := range entries {
		label, hasLabel := labels[entries[i].ID]
		node := &SessionTreeNode{Entry: entries[i], Children: []*SessionTreeNode{}}
		if hasLabel {
			node.Label = &label
		}
		nodes[entries[i].ID] = node
	}
	for i := range entries {
		node := nodes[entries[i].ID]
		if entries[i].ParentID == "" || entries[i].ParentID == entries[i].ID {
			roots = append(roots, node)
			continue
		}
		parent, ok := nodes[entries[i].ParentID]
		if !ok {
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	return rootsToValues(roots)
}

// rootsToValues converts allocated root nodes into the value slice returned by
// GetTree while keeping the child pointers intact.
func rootsToValues(roots []*SessionTreeNode) []SessionTreeNode {
	values := make([]SessionTreeNode, 0, len(roots))
	for _, root := range roots {
		values = append(values, *root)
	}
	return values
}

// ---------------------------------------------------------------------------
// Internal persistence
// ---------------------------------------------------------------------------

func (s *SessionManager) initNewSession(options *NewSessionOptions) {
	id := ""
	if options != nil && options.ID != nil {
		id = *options.ID
	}
	if id == "" {
		generated, err := aiutils.UUIDv7(nil)
		if err != nil || generated == "" {
			generated = randomHex(16)
		}
		id = generated
	}
	version := CurrentSessionVersion
	s.header = SessionHeader{
		Type:      "session",
		Version:   &version,
		ID:        id,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Cwd:       s.cwd,
	}
	if options != nil && options.ParentSession != nil {
		s.header.ParentSession = options.ParentSession
	}
	s.entries = nil
	s.byID = map[string]int{}
	s.labels = map[string]string{}
	s.leaf = ""
}

func (s *SessionManager) adopt(records []FileEntry) error {
	if len(records) == 0 || records[0].Header == nil {
		return fmt.Errorf("session file %s has no valid session header", s.file)
	}
	header := *records[0].Header
	s.header = header
	s.cwd = header.Cwd

	var entries []SessionEntry
	leaf := ""
	for _, record := range records {
		if record.Header != nil {
			continue
		}
		entry := record.Entry
		if entry == nil {
			continue
		}
		if entry.Type == leafRecordType {
			var leafID string
			if err := json.Unmarshal(entry.Payload, &leafID); err != nil {
				return fmt.Errorf("invalid leaf record: %w", err)
			}
			leaf = leafID
			continue
		}
		entries = append(entries, *entry)
		leaf = entry.ID
	}
	s.entries = entries
	s.leaf = leaf
	return s.rebuildIndex()
}

func (s *SessionManager) rebuildIndex() error {
	s.byID = make(map[string]int, len(s.entries))
	s.labels = map[string]string{}
	for i := range s.entries {
		entry := &s.entries[i]
		if entry.ID == "" {
			return fmt.Errorf("session entry of type %q is missing an id", entry.Type)
		}
		if entry.Type == "" {
			return fmt.Errorf("session entry %q is missing a type", entry.ID)
		}
		if _, duplicate := s.byID[entry.ID]; duplicate {
			return fmt.Errorf("duplicate session entry id %q", entry.ID)
		}
		s.byID[entry.ID] = i
	}
	if s.leaf != "" {
		if _, ok := s.byID[s.leaf]; !ok {
			return fmt.Errorf("session leaf %q does not exist", s.leaf)
		}
	}
	for i := range s.entries {
		entry := &s.entries[i]
		if entry.ParentID != "" {
			if _, ok := s.byID[entry.ParentID]; !ok {
				return fmt.Errorf("session entry %q references missing parent %q", entry.ID, entry.ParentID)
			}
		}
		seen := map[string]bool{}
		for current := entry.ID; current != ""; {
			if seen[current] {
				return fmt.Errorf("session entry cycle involving %q", current)
			}
			seen[current] = true
			index, ok := s.byID[current]
			if !ok {
				break
			}
			current = s.entries[index].ParentID
		}
	}
	for i := range s.entries {
		if s.entries[i].Type != "label" {
			continue
		}
		var payload struct {
			TargetID string  `json:"targetId"`
			Label    *string `json:"label"`
		}
		if err := json.Unmarshal(s.entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Label != nil {
			s.labels[payload.TargetID] = *payload.Label
		} else {
			delete(s.labels, payload.TargetID)
		}
	}
	return nil
}

func (s *SessionManager) branchPathLocked(leafID string) []SessionEntry {
	if leafID == "" {
		return nil
	}
	var path []SessionEntry
	current := leafID
	for current != "" {
		index, ok := s.byID[current]
		if !ok {
			break
		}
		path = append(path, s.entries[index])
		current = s.entries[index].ParentID
		if len(path) > len(s.entries) {
			break
		}
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

func (s *SessionManager) nextID() (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		id := randomHex(4)
		if _, exists := s.byID[id]; !exists {
			return id, nil
		}
	}
	return aiutils.UUIDv7(nil)
}

func (s *SessionManager) createFile() error {
	if err := os.MkdirAll(filepath.Dir(s.file), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s.header)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(s.file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func (s *SessionManager) appendRaw(record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return s.appendBytes(data)
}

func (s *SessionManager) appendBytes(data []byte) error {
	file, err := os.OpenFile(s.file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func (s *SessionManager) terminateFile() error {
	return s.appendBytes([]byte("\n"))
}

func (s *SessionManager) persistLeaf(leafID string) error {
	payload, err := json.Marshal(leafID)
	if err != nil {
		return err
	}
	return s.appendRaw(SessionEntry{Type: leafRecordType, Payload: payload})
}

// appendLocked is Append without re-acquiring the mutex.
func (s *SessionManager) appendLocked(kind string, payload json.RawMessage) (SessionEntry, error) {
	stored, err := normalizePayload(payload)
	if err != nil {
		return SessionEntry{}, err
	}
	id, err := s.nextID()
	if err != nil {
		return SessionEntry{}, err
	}
	entry := SessionEntry{ID: id, ParentID: s.leaf, Type: kind, Payload: stored}
	if s.persist {
		if err := s.appendRaw(entry); err != nil {
			return SessionEntry{}, err
		}
	}
	s.entries = append(s.entries, entry)
	s.byID[entry.ID] = len(s.entries) - 1
	s.leaf = entry.ID
	return cloneSessionEntry(entry), nil
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

type parseMeta struct {
	validPrefix      int
	recoveredTail    bool
	needsTermination bool
}

// ParseSessionEntries parses JSONL content into header and entry records.
//
// Unlike the upstream helper it reports malformed complete records instead of
// silently dropping them, matching the frozen SDK requirement that opening a
// malformed session fails visibly.
func ParseSessionEntries(content string) ([]FileEntry, error) {
	records, _, err := parseRecords([]byte(content), false)
	return records, err
}

// LoadEntriesFromFile reads and parses a session file. A missing file yields no
// entries. Malformed complete records fail; a truncated crash tail is skipped
// (the prefix is preserved and the caller may truncate it).
func LoadEntriesFromFile(path string) ([]FileEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []FileEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	records, _, err := parseRecords(data, true)
	return records, err
}

func parseRecords(data []byte, requireHeader bool) ([]FileEntry, parseMeta, error) {
	meta := parseMeta{validPrefix: len(data)}
	var records []FileEntry
	offset := 0
	for offset < len(data) {
		index := bytes.IndexByte(data[offset:], '\n')
		if index < 0 {
			fragment := data[offset:]
			if len(bytes.TrimSpace(fragment)) == 0 {
				break
			}
			record, err := parseRecord(fragment)
			if err != nil {
				meta.recoveredTail = true
				break
			}
			records = append(records, record)
			meta.needsTermination = true
			meta.validPrefix = len(data)
			break
		}
		line := data[offset : offset+index]
		next := offset + index + 1
		if len(bytes.TrimSpace(line)) > 0 {
			record, err := parseRecord(line)
			if err != nil {
				return nil, meta, fmt.Errorf("corrupt session record at byte %d: %w", offset, err)
			}
			records = append(records, record)
		}
		meta.validPrefix = next
		offset = next
	}
	if requireHeader {
		if err := validateSessionRecords(records); err != nil {
			return nil, meta, err
		}
	}
	return records, meta, nil
}

func validateSessionRecords(records []FileEntry) error {
	if len(records) == 0 {
		return nil
	}
	header := records[0].Header
	if header == nil || header.Type != "session" || header.ID == "" {
		return errors.New("session file does not start with a valid session header")
	}
	if header.Version == nil {
		return fmt.Errorf("unsupported session file version: missing (supported %d)", CurrentSessionVersion)
	}
	if *header.Version != CurrentSessionVersion {
		return fmt.Errorf(
			"unsupported session file version %d (supported %d)",
			*header.Version,
			CurrentSessionVersion,
		)
	}
	return nil
}

func parseRecord(line []byte) (FileEntry, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return FileEntry{}, err
	}
	if probe.Type == "session" {
		var header SessionHeader
		if err := json.Unmarshal(line, &header); err != nil {
			return FileEntry{}, err
		}
		return FileEntry{Header: &header}, nil
	}
	var entry SessionEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return FileEntry{}, err
	}
	if entry.Type == "" {
		return FileEntry{}, errors.New("session record is missing a type")
	}
	return FileEntry{Entry: &entry}, nil
}

// ---------------------------------------------------------------------------
// Context projection
// ---------------------------------------------------------------------------

// GetLatestCompactionEntry returns the last compaction entry, if any.
func GetLatestCompactionEntry(entries []SessionEntry) (SessionEntry, bool) {
	for index := len(entries) - 1; index >= 0; index-- {
		if entries[index].Type == "compaction" {
			return entries[index], true
		}
	}
	return SessionEntry{}, false
}

// BuildContextEntries returns the compaction-aware active entry list following
// the path from leafID to the root. A nil leafID uses the last entry; a pointer
// to an empty string selects the root.
func BuildContextEntries(entries []SessionEntry, leafID *string) []SessionEntry {
	byID := indexEntries(entries)
	path := buildSessionPath(entries, leafID, byID)
	compactionIndex := -1
	for index, entry := range path {
		if entry.Type == "compaction" {
			compactionIndex = index
		}
	}
	if compactionIndex < 0 {
		return path
	}
	compaction := path[compactionIndex]
	var payload struct {
		FirstKeptEntryID string `json:"firstKeptEntryId"`
	}
	_ = json.Unmarshal(compaction.Payload, &payload)
	contextEntries := []SessionEntry{compaction}
	foundFirstKept := false
	for index := 0; index < compactionIndex; index++ {
		entry := path[index]
		if entry.ID == payload.FirstKeptEntryID {
			foundFirstKept = true
		}
		if foundFirstKept && !(entry.Type == "message" && payloadRole(entry.Payload) == "system") {
			contextEntries = append(contextEntries, entry)
		}
	}
	contextEntries = append(contextEntries, path[compactionIndex+1:]...)
	return contextEntries
}

// SessionEntryToContextMessages projects one entry onto model messages.
// Plain "custom" entries are state and contribute nothing.
func SessionEntryToContextMessages(entry SessionEntry) []agenttypes.AgentMessage {
	switch entry.Type {
	case "message":
		message, err := decodeAgentMessage(entry.Payload)
		if err != nil {
			return nil
		}
		return []agenttypes.AgentMessage{message}
	case "custom_message":
		var payload struct {
			CustomType string          `json:"customType"`
			Content    json.RawMessage `json:"content"`
			Display    bool            `json:"display"`
			Details    json.RawMessage `json:"details"`
			Timestamp  float64         `json:"timestamp"`
		}
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return nil
		}
		var content aitypes.UserContent
		if len(payload.Content) > 0 && string(payload.Content) != "null" {
			if err := json.Unmarshal(payload.Content, &content); err != nil {
				return nil
			}
		}
		message := harnessMessage(
			harnessmessages.CreateCustomMessage(
				payload.CustomType,
				content,
				payload.Display,
				payload.Details,
				harnessmessages.TimestampFromNumber(payload.Timestamp),
			),
			CustomRole,
		)
		return []agenttypes.AgentMessage{message}
	case "branch_summary", "compaction":
		return projectSummaryEntry(entry)
	default:
		return nil
	}
}

func projectSummaryEntry(entry SessionEntry) []agenttypes.AgentMessage {
	var payload struct {
		Summary       string          `json:"summary"`
		FromID        *string         `json:"fromId"`
		TokensBefore  float64         `json:"tokensBefore"`
		Timestamp     float64         `json:"timestamp"`
		SystemMessage json.RawMessage `json:"systemMessage"`
	}
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		return nil
	}
	var messages []agenttypes.AgentMessage
	if entry.Type == "compaction" && len(payload.SystemMessage) > 0 {
		if system, err := decodeAgentMessage(payload.SystemMessage); err == nil {
			messages = append(messages, system)
		}
	}
	if entry.Type == "branch_summary" {
		if payload.Summary == "" {
			return messages
		}
		messages = append(messages, harnessMessage(
			harnessmessages.CreateBranchSummaryMessage(
				payload.Summary,
				payload.FromID,
				harnessmessages.TimestampFromNumber(payload.Timestamp),
			),
			BranchSummaryRole,
		))
		return messages
	}
	messages = append(messages, harnessMessage(
		harnessmessages.CreateCompactionSummaryMessage(
			payload.Summary,
			payload.TokensBefore,
			harnessmessages.TimestampFromNumber(payload.Timestamp),
		),
		CompactionSummaryRole,
	))
	return messages
}

// BuildSessionProjection builds the provenance-preserving context projection.
func BuildSessionProjection(entries []SessionEntry, leafID *string) SessionProjection {
	byID := indexEntries(entries)
	path := buildSessionPath(entries, leafID, byID)
	thinkingLevel, model := sessionContextSettings(path)
	contextEntries := BuildContextEntries(entries, leafID)

	edits := map[string]*SessionEntry{}
	for index := range contextEntries {
		entry := contextEntries[index]
		if entry.Type == "context_edit" {
			var payload struct {
				TargetID string `json:"targetId"`
			}
			if err := json.Unmarshal(entry.Payload, &payload); err == nil {
				edit := contextEntries[index]
				edits[payload.TargetID] = &edit
			}
		}
	}

	projectedEntries := make([]ProjectedSessionEntry, 0, len(contextEntries))
	var allMessages []agenttypes.AgentMessage
	for index, source := range contextEntries {
		var messages []agenttypes.AgentMessage
		if source.Type == "compaction" && index > 0 {
			messages = nil
		} else {
			messages = projectContextEntry(source, edits[source.ID])
		}
		projectedEntries = append(projectedEntries, ProjectedSessionEntry{
			SourceEntry: source,
			Messages:    messages,
		})
		allMessages = append(allMessages, messages...)
	}
	return SessionProjection{
		Entries:       projectedEntries,
		Messages:      allMessages,
		ThinkingLevel: thinkingLevel,
		Model:         model,
	}
}

// BuildSessionContext builds the finalized model context.
func BuildSessionContext(entries []SessionEntry, leafID *string) SessionContext {
	projection := BuildSessionProjection(entries, leafID)
	return SessionContext{
		Messages:      projection.Messages,
		ThinkingLevel: projection.ThinkingLevel,
		Model:         projection.Model,
	}
}

// MigrateSessionEntries brings in-memory file entries to the current version:
// the header version is bumped and legacy "hookMessage" roles become "custom".
// The durable loader only accepts version 3 files, so this is for callers that
// already hold parsed entries.
func MigrateSessionEntries(entries []FileEntry) []FileEntry {
	for index := range entries {
		entry := &entries[index]
		if entry.Header != nil {
			version := CurrentSessionVersion
			entry.Header.Version = &version
			continue
		}
		if entry.Entry == nil || entry.Entry.Type != "message" {
			continue
		}
		rewritten, ok := migrateMessagePayload(entry.Entry.Payload)
		if ok {
			entry.Entry.Payload = rewritten
		}
	}
	return entries
}

func migrateMessagePayload(payload json.RawMessage) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return payload, false
	}
	rawRole, ok := fields["role"]
	if !ok {
		return payload, false
	}
	var role string
	if err := json.Unmarshal(rawRole, &role); err != nil || role != "hookMessage" {
		return payload, false
	}
	fields["role"] = json.RawMessage(`"custom"`)
	rewritten, err := json.Marshal(fields)
	if err != nil {
		return payload, false
	}
	return rewritten, true
}

// ---------------------------------------------------------------------------
// Discovery helpers
// ---------------------------------------------------------------------------

// GetDefaultSessionDir computes the default session directory for a cwd.
func GetDefaultSessionDir(cwd, agentDir string) string {
	resolvedCwd := filepath.Clean(cwd)
	resolvedAgentDir := filepath.Clean(agentDir)
	stripped := strings.TrimLeft(resolvedCwd, `/\`)
	safePath := "--" + strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(stripped) + "--"
	return filepath.Join(resolvedAgentDir, "sessions", safePath)
}

// FindMostRecentSession returns the most recently modified session file whose
// stored cwd matches cwd (when provided). It never fails: unreadable files and
// directories simply yield no result.
func FindMostRecentSession(sessionDir string, cwd *string) (string, bool) {
	dirEntries, err := os.ReadDir(sessionDir)
	if err != nil {
		return "", false
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	candidates := make([]candidate, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() || !strings.HasSuffix(dirEntry.Name(), ".jsonl") {
			continue
		}
		info, err := dirEntry.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{
			path:    filepath.Join(sessionDir, dirEntry.Name()),
			modTime: info.ModTime(),
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	for _, item := range candidates {
		header, err := readSessionHeader(item.path)
		if err != nil || header == nil {
			continue
		}
		if cwd != nil && header.Cwd != "" && filepath.Clean(header.Cwd) != filepath.Clean(*cwd) {
			continue
		}
		return item.path, true
	}
	return "", false
}

func readSessionHeader(path string) (*SessionHeader, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	records, _, err := parseRecords(data, false)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 || records[0].Header == nil {
		return nil, nil
	}
	return records[0].Header, nil
}

// ---------------------------------------------------------------------------
// session-cwd helpers
// ---------------------------------------------------------------------------

// GetMissingSessionCwdIssue reports a stored session cwd that no longer exists.
func GetMissingSessionCwdIssue(sessionManager SessionCwdSource, fallbackCwd string) *SessionCwdIssue {
	file := sessionManager.SessionFile()
	if file == "" {
		return nil
	}
	cwd := sessionManager.GetCwd()
	if cwd == "" {
		return nil
	}
	if _, err := os.Stat(cwd); err == nil {
		return nil
	}
	fileCopy := file
	return &SessionCwdIssue{SessionFile: &fileCopy, SessionCwd: cwd, FallbackCwd: fallbackCwd}
}

// FormatMissingSessionCwdError renders the missing-cwd error text.
func FormatMissingSessionCwdError(issue SessionCwdIssue) string {
	sessionFile := ""
	if issue.SessionFile != nil {
		sessionFile = "\nSession file: " + *issue.SessionFile
	}
	return "Stored session working directory does not exist: " + issue.SessionCwd + sessionFile +
		"\nCurrent working directory: " + issue.FallbackCwd
}

// FormatMissingSessionCwdPrompt renders the missing-cwd prompt text.
func FormatMissingSessionCwdPrompt(issue SessionCwdIssue) string {
	return "cwd from session file does not exist\n" + issue.SessionCwd +
		"\n\ncontinue in current cwd\n" + issue.FallbackCwd
}

// AssertSessionCwdExists returns a MissingSessionCwdError when the stored cwd is
// gone.
func AssertSessionCwdExists(sessionManager SessionCwdSource, fallbackCwd string) error {
	issue := GetMissingSessionCwdIssue(sessionManager, fallbackCwd)
	if issue == nil {
		return nil
	}
	return &MissingSessionCwdError{Issue: *issue}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func randomHex(byteCount int) string {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func normalizePayload(payload json.RawMessage) (json.RawMessage, error) {
	if len(payload) == 0 {
		return json.RawMessage("null"), nil
	}
	if !json.Valid(payload) {
		return nil, errors.New("session entry payload must be valid JSON")
	}
	return append(json.RawMessage(nil), payload...), nil
}

func cloneSessionEntry(entry SessionEntry) SessionEntry {
	cloned := entry
	if entry.Payload != nil {
		cloned.Payload = append(json.RawMessage(nil), entry.Payload...)
	}
	return cloned
}

func cloneSessionEntries(entries []SessionEntry) []SessionEntry {
	cloned := make([]SessionEntry, len(entries))
	for index := range entries {
		cloned[index] = cloneSessionEntry(entries[index])
	}
	return cloned
}

func indexEntries(entries []SessionEntry) map[string]SessionEntry {
	index := make(map[string]SessionEntry, len(entries))
	for _, entry := range entries {
		index[entry.ID] = entry
	}
	return index
}

func buildSessionPath(entries []SessionEntry, leafID *string, byID map[string]SessionEntry) []SessionEntry {
	var leaf SessionEntry
	found := false
	if leafID == nil {
		if len(entries) > 0 {
			leaf = entries[len(entries)-1]
			found = true
		}
	} else if *leafID != "" {
		leaf, found = byID[*leafID]
	}
	if !found {
		return []SessionEntry{}
	}
	var reversed []SessionEntry
	current := leaf
	seen := map[string]bool{}
	for current.ID != "" && !seen[current.ID] {
		seen[current.ID] = true
		reversed = append(reversed, current)
		if current.ParentID == "" {
			break
		}
		next, ok := byID[current.ParentID]
		if !ok {
			break
		}
		current = next
	}
	path := make([]SessionEntry, len(reversed))
	for index := range reversed {
		path[index] = reversed[len(reversed)-1-index]
	}
	return path
}

func sessionContextSettings(path []SessionEntry) (string, *SessionModelRef) {
	thinkingLevel := "off"
	var model *SessionModelRef
	for _, entry := range path {
		switch entry.Type {
		case "thinking_level_change":
			var payload struct {
				ThinkingLevel string `json:"thinkingLevel"`
			}
			if err := json.Unmarshal(entry.Payload, &payload); err == nil && payload.ThinkingLevel != "" {
				thinkingLevel = payload.ThinkingLevel
			}
		case "model_change":
			var payload struct {
				Provider string `json:"provider"`
				ModelID  string `json:"modelId"`
			}
			if err := json.Unmarshal(entry.Payload, &payload); err == nil {
				model = &SessionModelRef{Provider: payload.Provider, ModelID: payload.ModelID}
			}
		case "message":
			if ref, ok := assistantModelRef(entry.Payload); ok {
				model = &ref
			}
		}
	}
	return thinkingLevel, model
}

func assistantModelRef(payload json.RawMessage) (SessionModelRef, bool) {
	message, err := decodeAgentMessage(payload)
	if err != nil || message.Message == nil || message.Message.Assistant == nil {
		return SessionModelRef{}, false
	}
	return SessionModelRef{
		Provider: string(message.Message.Assistant.Provider),
		ModelID:  message.Message.Assistant.Model,
	}, true
}

func projectContextEntry(entry SessionEntry, edit *SessionEntry) []agenttypes.AgentMessage {
	messages := SessionEntryToContextMessages(entry)
	if edit == nil {
		return messages
	}
	var payload struct {
		Replacement json.RawMessage `json:"replacement"`
	}
	if err := json.Unmarshal(edit.Payload, &payload); err != nil {
		return messages
	}
	if string(payload.Replacement) == "null" {
		return nil
	}
	var replacement ContextEditReplacement
	if err := json.Unmarshal(payload.Replacement, &replacement); err != nil {
		return messages
	}
	if replacement.Content == nil {
		return messages
	}
	updated := make([]agenttypes.AgentMessage, 0, len(messages))
	for _, message := range messages {
		updated = append(updated, applyContextReplacement(message, replacement.Content))
	}
	return updated
}

func applyContextReplacement(message agenttypes.AgentMessage, content json.RawMessage) agenttypes.AgentMessage {
	raw, err := json.Marshal(message)
	if err != nil {
		return message
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return message
	}
	fields["content"] = content
	updated, err := json.Marshal(fields)
	if err != nil {
		return message
	}
	var out agenttypes.AgentMessage
	if err := json.Unmarshal(updated, &out); err != nil {
		return message
	}
	return out
}

func decodeAgentMessage(payload json.RawMessage) (agenttypes.AgentMessage, error) {
	var message agenttypes.AgentMessage
	if len(payload) == 0 {
		return message, errors.New("empty message payload")
	}
	if err := json.Unmarshal(payload, &message); err != nil {
		return message, err
	}
	return message, nil
}

func harnessMessage(value any, role string) agenttypes.AgentMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return agenttypes.AgentMessage{}
	}
	return agenttypes.NewCustomMessage(role, raw)
}

func payloadRole(payload json.RawMessage) string {
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return ""
	}
	return probe.Role
}
