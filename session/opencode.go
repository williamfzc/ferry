// This file implements the OpenCode session export format and storage layout.
// OpenCode CLI exchanges sessions as one JSON document (`opencode export` /
// `opencode import`); its on-disk store splits a session across
// session/message/part files. Read accepts the export document and the
// archive form Ferry captures for storage-discovered sessions; Write emits
// the export document against the import schema observed on CLI 1.18.32.
package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// OpenCode is the OpenCode CLI session codec.
type OpenCode struct{}

type openCodeTimeSpan struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
}

type openCodeInfo struct {
	ID        string           `json:"id"`
	Directory string           `json:"directory"`
	Time      openCodeTimeSpan `json:"time"`
}

type openCodeMessageInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	Time      struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

type openCodeMessage struct {
	Info  openCodeMessageInfo `json:"info"`
	Parts []json.RawMessage   `json:"parts"`
}

// openCodeStoredMessage is the flat record kept in one message file under a
// session's storage directory; the export document wraps the same fields in
// an "info" object instead.
type openCodeStoredMessage struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	Time      struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

type openCodeDoc struct {
	Info     openCodeInfo      `json:"info"`
	Messages []openCodeMessage `json:"messages"`
}

// openCodeArchiveEntry stores one storage file byte-for-byte. Storage
// sessions span many files, so the captured original is this line-oriented
// archive instead of a single native stream. Data is raw bytes (JSON
// base64), which keeps even non-UTF-8 file content exact.
type openCodeArchiveEntry struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

func (OpenCode) Read(r io.Reader) (Session, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Session{}, err
	}
	s, err := readOpenCodeBytes(data)
	if err != nil {
		return s, err
	}
	s.Original = data
	return s, nil
}

func readOpenCodeBytes(data []byte) (Session, error) {
	s := Session{SourceType: "opencode"}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return s, fmt.Errorf("empty OpenCode session")
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &shape); err == nil {
		if _, ok := shape["messages"]; ok {
			return openCodeSessionFromDoc(data)
		}
	}
	if looksLikeOpenCodeArchive(trimmed) {
		return openCodeSessionFromArchive(trimmed)
	}
	return s, fmt.Errorf("not an OpenCode export document or session archive")
}

func looksLikeOpenCodeArchive(data []byte) bool {
	first := data
	if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
		first = data[:idx]
	}
	var entry openCodeArchiveEntry
	return json.Unmarshal(bytes.TrimSpace(first), &entry) == nil && entry.Path != "" && entry.Data != nil
}

func openCodeSessionFromDoc(data []byte) (Session, error) {
	var doc openCodeDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return Session{SourceType: "opencode"}, fmt.Errorf("invalid OpenCode export document: %w", err)
	}
	return assembleOpenCodeSession(doc.Info, doc.Messages)
}

type openCodeArchiveContents struct {
	Info     openCodeInfo
	Messages []openCodeMessage
}

func openCodeSessionFromArchive(data []byte) (Session, error) {
	s := Session{SourceType: "opencode"}
	contents, err := openCodeReadArchive(data)
	if err != nil {
		return s, err
	}
	return assembleOpenCodeSession(contents.Info, contents.Messages)
}

func openCodeReadArchive(data []byte) (openCodeArchiveContents, error) {
	var contents openCodeArchiveContents
	current := -1
	seenSession := false
	for line, raw := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var entry openCodeArchiveEntry
		if err := json.Unmarshal(raw, &entry); err != nil || entry.Path == "" {
			return contents, fmt.Errorf("archive line %d is not a file entry", line+1)
		}
		fileBytes := entry.Data
		switch {
		case !seenSession && strings.HasPrefix(entry.Path, "session/"):
			if err := json.Unmarshal(fileBytes, &contents.Info); err != nil {
				return contents, fmt.Errorf("archive session file %q is invalid: %w", entry.Path, err)
			}
			seenSession = true
		case strings.HasPrefix(entry.Path, "message/"):
			var stored openCodeStoredMessage
			if err := json.Unmarshal(fileBytes, &stored); err != nil {
				return contents, fmt.Errorf("archive message file %q is invalid: %w", entry.Path, err)
			}
			msg := openCodeMessage{Info: openCodeMessageInfo{ID: stored.ID, SessionID: stored.SessionID, Role: stored.Role}}
			msg.Info.Time.Created = stored.Time.Created
			contents.Messages = append(contents.Messages, msg)
			current = len(contents.Messages) - 1
		case strings.HasPrefix(entry.Path, "part/"):
			if current < 0 {
				return contents, fmt.Errorf("archive part file %q appears before any message", entry.Path)
			}
			if owner := "part/" + contents.Messages[current].Info.ID + "/"; !strings.HasPrefix(entry.Path, owner) {
				return contents, fmt.Errorf("archive part file %q does not belong to message %q", entry.Path, contents.Messages[current].Info.ID)
			}
			contents.Messages[current].Parts = append(contents.Messages[current].Parts, json.RawMessage(fileBytes))
		default:
			return contents, fmt.Errorf("archive entry %q is not a session, message, or part file", entry.Path)
		}
	}
	if !seenSession {
		return contents, fmt.Errorf("archive has no session file")
	}
	return contents, nil
}

// assembleOpenCodeSession maps an assembled OpenCode session onto the common
// model. Progress markers (step-start, step-finish) and reasoning parts carry
// no cross-provider content and are omitted; the limitation reporting in
// transfer.go counts them from the original bytes.
func assembleOpenCodeSession(info openCodeInfo, messages []openCodeMessage) (Session, error) {
	s := Session{SourceType: "opencode", NativeID: info.ID, Workspace: info.Directory}
	if s.NativeID == "" {
		return s, fmt.Errorf("OpenCode session has no id")
	}
	if info.Time.Updated != 0 {
		s.Updated = time.UnixMilli(info.Time.Updated)
	}
	for _, msg := range messages {
		if msg.Info.SessionID != "" && msg.Info.SessionID != info.ID {
			return s, fmt.Errorf("OpenCode message %q belongs to session %q, not %q", msg.Info.ID, msg.Info.SessionID, info.ID)
		}
		if msg.Info.Role != "user" && msg.Info.Role != "assistant" {
			continue
		}
		timestamp := openCodeTimestamp(msg.Info.Time.Created)
		m := Message{Role: msg.Info.Role, Timestamp: timestamp}
		for partIndex, raw := range msg.Parts {
			blocks, resultEnd, err := openCodeContentParts(raw)
			if err != nil {
				return s, fmt.Errorf("OpenCode message %q part %d: %w", msg.Info.ID, partIndex, err)
			}
			// An OpenCode tool part carries the call and its result in one
			// object. The common model keeps results in their own tool-role
			// message so every writer can represent them, matching how the
			// Codex reader splits function calls from their outputs. The
			// result message takes the tool's completion time, which is also
			// what the writer records for a merged result.
			if len(blocks) == 2 && blocks[0].Type == "tool_call" && blocks[1].Type == "tool_result" {
				m.Parts = append(m.Parts, blocks[0])
				s.Messages = append(s.Messages, m)
				resultTimestamp := timestamp
				if resultEnd != 0 {
					resultTimestamp = openCodeTimestamp(resultEnd)
				}
				s.Messages = append(s.Messages, Message{Role: "tool", Timestamp: resultTimestamp, Parts: []ContentBlock{blocks[1]}})
				m = Message{Role: msg.Info.Role, Timestamp: timestamp}
				continue
			}
			m.Parts = append(m.Parts, blocks...)
		}
		if len(m.Parts) > 0 {
			s.Messages = append(s.Messages, m)
		}
	}
	return s, nil
}

type openCodePart struct {
	Type   string          `json:"type"`
	Text   string          `json:"text"`
	CallID string          `json:"callID"`
	Tool   string          `json:"tool"`
	State  json.RawMessage `json:"state"`
}

type openCodeToolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
	Error  string          `json:"error"`
	Time   struct {
		End int64 `json:"end"`
	} `json:"time"`
}

// openCodeContentParts maps one part onto common blocks. The second return
// is the tool completion time when the part carries a finished call.
func openCodeContentParts(raw json.RawMessage) ([]ContentBlock, int64, error) {
	var part openCodePart
	if err := json.Unmarshal(raw, &part); err != nil || part.Type == "" {
		return nil, 0, nil
	}
	switch part.Type {
	case "text":
		return []ContentBlock{{Type: "text", Text: part.Text, Raw: raw}}, 0, nil
	case "tool":
		if err := validateOpenCodeToolState(part); err != nil {
			return nil, 0, err
		}
		var state openCodeToolState
		if err := json.Unmarshal(part.State, &state); err != nil {
			return nil, 0, fmt.Errorf("OpenCode tool part %q has invalid state: %w", part.CallID, err)
		}
		call := &ToolCall{ID: part.CallID, Name: part.Tool, Input: state.Input, Status: openCodeToolCallStatus(state.Status)}
		if call.ID == "" || call.Name == "" {
			return nil, 0, nil
		}
		blocks := []ContentBlock{{Type: "tool_call", ToolCall: call, Raw: raw}}
		if result, ok := openCodeToolResult(part.CallID, state); ok {
			blocks = append(blocks, ContentBlock{Type: "tool_result", ToolResult: result})
		}
		return blocks, state.Time.End, nil
	default:
		return nil, 0, nil
	}
}

// validateOpenCodeToolState checks the structural part of OpenCode's tool
// state before it is converted to the common model. A tool part without a
// state is otherwise easy to mistake for a pending call: json.Unmarshal
// accepts null into a struct, and the caller would then silently omit the
// part's state-dependent content. Unknown status values remain valid here so
// future provider states can be preserved in Raw and reported as a
// limitation, but the state must still be an object with a status and input.
func validateOpenCodeToolState(part openCodePart) error {
	state := bytes.TrimSpace(part.State)
	if len(state) == 0 || bytes.Equal(state, []byte("null")) {
		return fmt.Errorf("OpenCode tool part %q is missing state", part.CallID)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(state, &fields); err != nil || fields == nil {
		if err == nil {
			return fmt.Errorf("OpenCode tool part %q has invalid state: expected an object", part.CallID)
		}
		return fmt.Errorf("OpenCode tool part %q has invalid state: expected an object: %w", part.CallID, err)
	}
	status, ok := fields["status"]
	if !ok || len(bytes.TrimSpace(status)) == 0 || bytes.Equal(bytes.TrimSpace(status), []byte("null")) {
		return fmt.Errorf("OpenCode tool part %q has invalid state: status is required", part.CallID)
	}
	var statusValue string
	if err := json.Unmarshal(status, &statusValue); err != nil || statusValue == "" {
		if err == nil {
			return fmt.Errorf("OpenCode tool part %q has invalid state: status must be a non-empty string", part.CallID)
		}
		return fmt.Errorf("OpenCode tool part %q has invalid state: status must be a non-empty string: %w", part.CallID, err)
	}
	if input, ok := fields["input"]; !ok || len(bytes.TrimSpace(input)) == 0 || bytes.Equal(bytes.TrimSpace(input), []byte("null")) {
		return fmt.Errorf("OpenCode tool part %q has invalid state: input is required", part.CallID)
	}
	statusValue = strings.TrimSpace(statusValue)
	for _, field := range []string{"output", "error"} {
		required := (statusValue == "completed" && field == "output") || (statusValue == "error" && field == "error")
		if required {
			value, ok := fields[field]
			if !ok || len(bytes.TrimSpace(value)) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("OpenCode tool part %q has invalid state: %s is required for status %q", part.CallID, field, statusValue)
			}
		}
	}
	return nil
}

// openCodeToolCallStatus maps only OpenCode's documented tool states to their
// ACP equivalents. Unknown provider states stay in the raw content block
// instead of being guessed as a completed or failed call.
func openCodeToolCallStatus(status string) string {
	switch status {
	case "pending":
		return "pending"
	case "running":
		return "in_progress"
	case "completed":
		return "completed"
	case "error":
		return "failed"
	default:
		return ""
	}
}

// openCodeToolResult derives the result side of a tool part. A completed
// state reports its output; an error state is a failed execution and keeps
// the error flag so writers with an error marker can preserve it.
func openCodeToolResult(callID string, state openCodeToolState) (*ToolResult, bool) {
	if state.Status != "completed" && state.Status != "error" {
		return nil, false
	}
	result := &ToolResult{ID: callID}
	if state.Status == "error" {
		result.IsError = true
		result.Content = state.Error
	}
	// json.Unmarshal(null, &output) succeeds and produces an empty string.
	// Preserve an explicit null output as structured content instead of
	// silently treating it as an empty result.
	if result.Content == "" && isJSONNull(state.Output) {
		result.Content = "null"
		result.RawContent = bytes.Clone(state.Output)
		return result, true
	}
	if result.Content == "" {
		var output string
		if err := json.Unmarshal(state.Output, &output); err != nil {
			result.RawContent = state.Output
			return result, true
		}
		result.Content = output
	}
	return result, true
}

func openCodeTimestamp(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func openCodeMillis(timestamp string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp %q: %w", timestamp, err)
	}
	return t.UnixMilli(), nil
}

// parseOpenCodeFile reads one local file: an export document, a saved
// archive, or a session file inside an OpenCode storage tree.
func parseOpenCodeFile(path string) (Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Session{SourceType: "opencode", SourcePath: path}, err
	}
	var shape map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(data), &shape) == nil {
		if _, ok := shape["messages"]; ok {
			s, err := openCodeSessionFromDoc(data)
			if err == nil {
				s.Original = data
			}
			s.SourcePath = path
			return s, err
		}
	}
	if looksLikeOpenCodeArchive(data) {
		s, err := openCodeSessionFromArchive(data)
		if err == nil {
			s.Original = data
		}
		s.SourcePath = path
		return s, err
	}
	return assembleOpenCodeStorage(path, data)
}

// assembleOpenCodeStorage reads the sibling message and part files that make
// up a stored OpenCode session and captures every file byte in an archive.
func assembleOpenCodeStorage(path string, sessionBytes []byte) (Session, error) {
	s := Session{SourceType: "opencode", SourcePath: path}
	var info openCodeInfo
	if err := json.Unmarshal(sessionBytes, &info); err != nil {
		return s, fmt.Errorf("%s: invalid OpenCode session file: %w", path, err)
	}
	if info.ID == "" {
		return s, fmt.Errorf("%s: OpenCode session file has no id", path)
	}
	s.NativeID = info.ID
	s.Workspace = info.Directory
	if info.Time.Updated != 0 {
		s.Updated = time.UnixMilli(info.Time.Updated)
	}
	// Trust the layout only when it matches the real store; a session file
	// elsewhere must not silently produce an empty session.
	if filepath.Base(filepath.Dir(filepath.Dir(path))) != "session" {
		return s, fmt.Errorf("%s: not inside an OpenCode storage tree (…/storage/session/<project>/)", path)
	}
	storageRoot := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	messageDir := filepath.Join(storageRoot, "message", info.ID)
	entries := []openCodeArchiveEntry{{
		Path: openCodeStoragePath(storageRoot, path),
		Data: sessionBytes,
	}}
	messageFiles, err := filepath.Glob(filepath.Join(messageDir, "*.json"))
	if err != nil {
		return s, err
	}
	sort.Strings(messageFiles)
	seenMessages := make(map[string]bool)
	var messages []openCodeMessage
	for _, messageFile := range messageFiles {
		messageBytes, err := os.ReadFile(messageFile)
		if err != nil {
			return s, err
		}
		var stored openCodeStoredMessage
		if err := json.Unmarshal(messageBytes, &stored); err != nil {
			return s, fmt.Errorf("%s: invalid OpenCode message file: %w", messageFile, err)
		}
		if stored.ID == "" {
			return s, fmt.Errorf("%s: OpenCode message file has no id", messageFile)
		}
		if seenMessages[stored.ID] {
			return s, fmt.Errorf("%s: duplicate OpenCode message id %q", messageFile, stored.ID)
		}
		seenMessages[stored.ID] = true
		if stored.SessionID != "" && stored.SessionID != info.ID {
			return s, fmt.Errorf("%s: message belongs to session %q, not %q", messageFile, stored.SessionID, info.ID)
		}
		msg := openCodeMessage{Info: openCodeMessageInfo{ID: stored.ID, SessionID: stored.SessionID, Role: stored.Role}}
		msg.Info.Time.Created = stored.Time.Created
		entries = append(entries, openCodeArchiveEntry{
			Path: openCodeStoragePath(storageRoot, messageFile),
			Data: messageBytes,
		})
		partFiles, err := filepath.Glob(filepath.Join(storageRoot, "part", stored.ID, "*.json"))
		if err != nil {
			return s, err
		}
		sort.Strings(partFiles)
		for _, partFile := range partFiles {
			partBytes, err := os.ReadFile(partFile)
			if err != nil {
				return s, err
			}
			entries = append(entries, openCodeArchiveEntry{
				Path: openCodeStoragePath(storageRoot, partFile),
				Data: partBytes,
			})
			msg.Parts = append(msg.Parts, json.RawMessage(partBytes))
		}
		messages = append(messages, msg)
	}
	s.Original = openCodeArchiveBytes(entries)
	assembled, err := assembleOpenCodeSession(info, messages)
	if err != nil {
		return s, err
	}
	assembled.SourcePath = path
	assembled.Original = s.Original
	return assembled, nil
}

func openCodeStoragePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

func openCodeArchiveBytes(entries []openCodeArchiveEntry) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, entry := range entries {
		if err := enc.Encode(entry); err != nil {
			continue
		}
	}
	return buf.Bytes()
}

type openCodeOutModel struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

type openCodeOutPath struct {
	Cwd  string `json:"cwd"`
	Root string `json:"root"`
}

type openCodeOutTokenCache struct {
	Read  int64 `json:"read"`
	Write int64 `json:"write"`
}

type openCodeOutTokens struct {
	Input     int64                 `json:"input"`
	Output    int64                 `json:"output"`
	Reasoning int64                 `json:"reasoning"`
	Cache     openCodeOutTokenCache `json:"cache"`
}

type openCodeOutUserTime struct {
	Created int64 `json:"created"`
}

type openCodeOutAssistantTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed"`
}

// The import schema discriminates message info by role: user messages carry
// a nested model object, assistant messages carry flat model fields plus
// mode, path, cost, and tokens. Both shapes were confirmed against
// `opencode import` on CLI 1.18.32.
type openCodeOutUser struct {
	ID        string              `json:"id"`
	SessionID string              `json:"sessionID"`
	Role      string              `json:"role"`
	Agent     string              `json:"agent"`
	Model     openCodeOutModel    `json:"model"`
	ParentID  string              `json:"parentID,omitempty"`
	Time      openCodeOutUserTime `json:"time"`
}

type openCodeOutAssistant struct {
	ID         string                   `json:"id"`
	SessionID  string                   `json:"sessionID"`
	Role       string                   `json:"role"`
	Agent      string                   `json:"agent"`
	ModelID    string                   `json:"modelID"`
	ProviderID string                   `json:"providerID"`
	Mode       string                   `json:"mode"`
	Path       openCodeOutPath          `json:"path"`
	Cost       float64                  `json:"cost"`
	Tokens     openCodeOutTokens        `json:"tokens"`
	ParentID   string                   `json:"parentID,omitempty"`
	Time       openCodeOutAssistantTime `json:"time"`
}

type openCodeOutTextPart struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
	Text      string `json:"text"`
}

type openCodeOutToolTime struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type openCodeOutToolState struct {
	Status   string              `json:"status"`
	Input    json.RawMessage     `json:"input"`
	Output   string              `json:"output"`
	Error    string              `json:"error,omitempty"`
	Title    string              `json:"title"`
	Metadata map[string]string   `json:"metadata"`
	Time     openCodeOutToolTime `json:"time"`
}

type openCodeOutToolPart struct {
	ID        string               `json:"id"`
	SessionID string               `json:"sessionID"`
	MessageID string               `json:"messageID"`
	Type      string               `json:"type"`
	CallID    string               `json:"callID"`
	Tool      string               `json:"tool"`
	State     openCodeOutToolState `json:"state"`
}

type openCodeOutMessage struct {
	Info  any `json:"info"`
	Parts any `json:"parts"`
}

type openCodeOutInfo struct {
	ID        string           `json:"id"`
	Slug      string           `json:"slug"`
	Title     string           `json:"title"`
	Version   string           `json:"version"`
	Directory string           `json:"directory"`
	Time      openCodeTimeSpan `json:"time"`
}

type openCodeOutDoc struct {
	Info     openCodeOutInfo      `json:"info"`
	Messages []openCodeOutMessage `json:"messages"`
}

// pendingCall tracks an OpenCode tool part whose result has not been merged.
type pendingCall struct {
	part    *openCodeOutToolPart
	blocked bool
}

func removePendingCall(open []*pendingCall, done *pendingCall) []*pendingCall {
	for i, entry := range open {
		if entry == done {
			return append(open[:i], open[i+1:]...)
		}
	}
	return open
}

func (OpenCode) Write(w io.Writer, s Session) error {
	doc, err := buildOpenCodeDoc(s)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// buildOpenCodeDoc validates the whole session before any bytes are emitted.
// A tool call and its result become one OpenCode tool part placed at the
// call's position; a result whose call is missing is an error rather than a
// silent drop.
func buildOpenCodeDoc(s Session) (openCodeOutDoc, error) {
	doc := openCodeOutDoc{}
	if s.NativeID == "" {
		return doc, fmt.Errorf("native ID is required")
	}
	if s.Workspace == "" {
		return doc, fmt.Errorf("OpenCode workspace is required")
	}
	if len(s.Messages) == 0 {
		return doc, fmt.Errorf("no messages to write")
	}
	// OpenCode needs a millisecond clock for every message. Like the Codex
	// writer, a missing timestamp is carried forward from the previous
	// message instead of being invented from nothing.
	times := make([]int64, len(s.Messages))
	var last int64
	for i, m := range s.Messages {
		if m.Timestamp == "" {
			times[i] = last
			continue
		}
		ms, err := openCodeMillis(m.Timestamp)
		if err != nil {
			return doc, fmt.Errorf("message %d: %w", i, err)
		}
		times[i] = ms
		last = ms
	}
	if times[0] == 0 {
		if s.Updated.IsZero() {
			return doc, fmt.Errorf("message 0: timestamp is required")
		}
		times[0] = s.Updated.UnixMilli()
	}
	for i := 1; i < len(times); i++ {
		if times[i] == 0 {
			times[i] = times[i-1]
		}
	}
	updated := times[len(times)-1]
	if !s.Updated.IsZero() {
		updated = s.Updated.UnixMilli()
	}
	doc.Info = openCodeOutInfo{
		ID: s.NativeID, Slug: "ferry", Title: "ferry", Version: "ferry",
		Directory: s.Workspace, Time: openCodeTimeSpan{Created: times[0], Updated: updated},
	}
	calls := make(map[string]*pendingCall)
	var open []*pendingCall
	partSeq := 0
	messageID := func() string {
		partSeq++
		return fmt.Sprintf("msg_ferry%08d", partSeq)
	}
	partID := func() string {
		partSeq++
		return fmt.Sprintf("prt_ferry%08d", partSeq)
	}
	parent := ""
	for i, m := range s.Messages {
		if m.Role == "summary" {
			return doc, fmt.Errorf("message %d: summary cannot be represented by OpenCode", i)
		}
		if m.Role != "user" && m.Role != "assistant" && m.Role != "tool" {
			return doc, fmt.Errorf("message %d: unsupported role %q", i, m.Role)
		}
		if len(m.Parts) == 0 {
			return doc, fmt.Errorf("message %d: empty message", i)
		}
		created := times[i]
		var visible []any
		for j, p := range m.Parts {
			switch p.Type {
			case "text":
				if m.Role == "tool" {
					return doc, fmt.Errorf("message %d part %d: tool text needs a tool result", i, j)
				}
				visible = append(visible, p.Text)
				// OpenCode couples a call and its result in one part, so a
				// result can only attach to a call that no other content has
				// followed. Text blocks every still-open call; other calls
				// do not, which keeps parallel tool calls convertible.
				for _, open := range calls {
					if open.part.State.Status == "pending" {
						open.blocked = true
					}
				}
			case "tool_call":
				if p.ToolCall == nil || p.ToolCall.ID == "" || p.ToolCall.Name == "" || m.Role != "assistant" {
					return doc, fmt.Errorf("message %d part %d: invalid tool call", i, j)
				}
				input, _, err := wrapToolInputForCall(p)
				if err != nil {
					return doc, fmt.Errorf("message %d part %d: %w", i, j, err)
				}
				if _, exists := calls[p.ToolCall.ID]; exists {
					return doc, fmt.Errorf("message %d part %d: duplicate tool call id %q", i, j, p.ToolCall.ID)
				}
				toolPart := &openCodeOutToolPart{
					Type: "tool", CallID: p.ToolCall.ID, Tool: qualifiedToolName(p.ToolCall),
					State: openCodeOutToolState{Status: "pending", Input: input, Title: "ferry", Metadata: map[string]string{}, Time: openCodeOutToolTime{Start: created, End: created}},
				}
				entry := &pendingCall{part: toolPart}
				calls[p.ToolCall.ID] = entry
				open = append(open, entry)
				visible = append(visible, toolPart)
			case "tool_result":
				if p.ToolResult == nil || p.ToolResult.ID == "" || (m.Role != "user" && m.Role != "tool") {
					return doc, fmt.Errorf("message %d part %d: invalid tool result", i, j)
				}
				previous, ok := calls[p.ToolResult.ID]
				if !ok {
					return doc, fmt.Errorf("message %d part %d: tool result %q has no earlier call", i, j, p.ToolResult.ID)
				}
				if previous.part.State.Status != "pending" {
					return doc, fmt.Errorf("message %d part %d: duplicate result for tool call %q", i, j, p.ToolResult.ID)
				}
				if previous.blocked {
					return doc, fmt.Errorf("message %d part %d: tool result %q is separated from its call by other content, which OpenCode cannot reorder", i, j, p.ToolResult.ID)
				}
				content, err := toolResultText(p.ToolResult)
				if err != nil {
					return doc, fmt.Errorf("message %d part %d: %w", i, j, err)
				}
				if p.ToolResult.IsError {
					previous.part.State.Status = "error"
					previous.part.State.Error = content
				} else {
					previous.part.State.Output = content
					previous.part.State.Status = "completed"
				}
				previous.part.State.Time.End = created
				open = removePendingCall(open, previous)
			default:
				return doc, fmt.Errorf("message %d part %d: unsupported content %q", i, j, p.Type)
			}
		}
		if len(visible) == 0 {
			continue
		}
		id := messageID()
		parts := make([]any, 0, len(visible))
		for _, item := range visible {
			switch value := item.(type) {
			case string:
				parts = append(parts, openCodeOutTextPart{ID: partID(), SessionID: s.NativeID, MessageID: id, Type: "text", Text: value})
			case *openCodeOutToolPart:
				// Keep the pointer: a later tool_result message fills in this
				// part's state after it has been placed in the document.
				value.ID = partID()
				value.SessionID = s.NativeID
				value.MessageID = id
				parts = append(parts, value)
			}
		}
		if m.Role == "user" {
			doc.Messages = append(doc.Messages, openCodeOutMessage{Info: openCodeOutUser{
				ID: id, SessionID: s.NativeID, Role: "user", Agent: "build",
				Model:    openCodeOutModel{ProviderID: "ferry", ModelID: "ferry"},
				ParentID: parent, Time: openCodeOutUserTime{Created: created},
			}, Parts: parts})
		} else {
			doc.Messages = append(doc.Messages, openCodeOutMessage{Info: openCodeOutAssistant{
				ID: id, SessionID: s.NativeID, Role: "assistant", Agent: "build",
				ModelID: "ferry", ProviderID: "ferry", Mode: "build",
				Path:     openCodeOutPath{Cwd: s.Workspace, Root: s.Workspace},
				ParentID: parent, Time: openCodeOutAssistantTime{Created: created, Completed: created},
			}, Parts: parts})
		}
		parent = id
	}
	// An unfinished call cannot be written: the import schema demands the raw
	// provider payload for pending states, which the common model never had.
	for id, call := range calls {
		if call.part.State.Status == "pending" {
			return doc, fmt.Errorf("tool call %q has no result; OpenCode cannot represent an unfinished call", id)
		}
	}
	return doc, nil
}

// skippedOpenCodeLimitations reports OpenCode records that a target
// projection cannot represent: messages with roles outside user/assistant
// and parts other than text and tool, such as reasoning and step markers.
func skippedOpenCodeLimitations(s Session) []string {
	messages := openCodeMessagesForAnalysis(s.Original)
	roleCounts := make(map[string]int)
	partCounts := make(map[string]int)
	for _, msg := range messages {
		if msg.Info.Role != "user" && msg.Info.Role != "assistant" {
			name := msg.Info.Role
			if name == "" {
				name = "<missing role>"
			}
			roleCounts[name]++
		}
		for _, raw := range msg.Parts {
			var part openCodePart
			if err := json.Unmarshal(raw, &part); err != nil || part.Type == "" {
				partCounts["<invalid part>"]++
				continue
			}
			switch {
			case part.Type == "text":
			case part.Type == "tool" && part.CallID != "" && part.Tool != "":
			default:
				partCounts[part.Type]++
			}
		}
	}
	limitations := make([]string, 0, 2)
	if text := formatOpenCodeCounts("message roles", roleCounts); text != "" {
		limitations = append(limitations, text)
	}
	if text := formatOpenCodeCounts("part types", partCounts); text != "" {
		limitations = append(limitations, text)
	}
	return limitations
}

func openCodeMessagesForAnalysis(original []byte) []openCodeMessage {
	var doc openCodeDoc
	if err := json.Unmarshal(bytes.TrimSpace(original), &doc); err == nil && doc.Info.ID != "" && doc.Messages != nil {
		return doc.Messages
	}
	if contents, err := openCodeReadArchive(original); err == nil {
		return contents.Messages
	}
	return nil
}

func formatOpenCodeCounts(label string, counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	types := make([]string, 0, len(counts))
	for name := range counts {
		types = append(types, name)
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, name := range types {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return fmt.Sprintf("OpenCode source contains unrepresented %s (%s); the original bytes retain them.", label, strings.Join(parts, ", "))
}
