// This file implements Pi's append-only JSONL session format and active-branch projection.
// Pi stores all branches in one file; the common session model exposes the current branch.
package session

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Pi is the Pi coding agent session codec.
type Pi struct{}

type piHeader struct {
	Type          string `json:"type"`
	Version       int    `json:"version"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}

type piEntry struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	ParentID         json.RawMessage `json:"parentId"`
	Timestamp        string          `json:"timestamp"`
	Message          json.RawMessage `json:"message"`
	Summary          string          `json:"summary"`
	FromID           string          `json:"fromId"`
	FirstKeptEntryID string          `json:"firstKeptEntryId"`
	FirstKeptIndex   *int            `json:"firstKeptEntryIndex"`
}

type piMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	IsError    bool            `json:"isError"`
}

// Read reads a Pi session, retaining the exact input bytes. The common model
// contains the branch ending at the last entry, which is Pi's active leaf.
func (Pi) Read(r io.Reader) (Session, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Session{SourceType: "pi"}, err
	}
	s, err := readPiBytes(data)
	if err != nil {
		return s, err
	}
	s.Original = bytes.Clone(data)
	return s, nil
}

func readPiBytes(data []byte) (Session, error) {
	s := Session{SourceType: "pi"}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 32*1024*1024)
	var header piHeader
	var entries []piEntry
	line := 0
	headerSeen := false
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		if !headerSeen {
			if err := json.Unmarshal(raw, &header); err != nil {
				return s, fmt.Errorf("Pi session:%d: %w", line, err)
			}
			if header.Type != "session" {
				return s, fmt.Errorf("Pi session:%d: first record is not a session header", line)
			}
			if header.ID == "" {
				return s, fmt.Errorf("Pi session:%d: session header has no id", line)
			}
			headerSeen = true
			continue
		}
		var entry piEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return s, fmt.Errorf("Pi session:%d: %w", line, err)
		}
		if entry.Type == "session" {
			return s, fmt.Errorf("Pi session:%d: duplicate session header", line)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return s, err
	}
	if !headerSeen {
		return s, fmt.Errorf("Pi session is empty")
	}

	s.NativeID = header.ID
	s.Workspace = header.CWD
	if header.Timestamp == "" {
		return s, fmt.Errorf("Pi session header has no timestamp")
	}
	if _, err := time.Parse(time.RFC3339Nano, header.Timestamp); err != nil {
		return s, fmt.Errorf("Pi session header timestamp is invalid: %w", err)
	}
	s.Updated = piLatestTime(header.Timestamp, entries)
	path, err := piActivePath(entries, header.Version)
	if err != nil {
		return s, err
	}
	path = piContextPath(path)
	for _, entry := range path {
		messages, err := piEntryMessages(entry)
		if err != nil {
			return s, err
		}
		s.Messages = append(s.Messages, messages...)
	}
	return s, nil
}

func piLatestTime(headerTimestamp string, entries []piEntry) time.Time {
	latest, _ := time.Parse(time.RFC3339Nano, headerTimestamp)
	for _, entry := range entries {
		if current, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err == nil && current.After(latest) {
			latest = current
		}
	}
	return latest
}

func piActivePath(entries []piEntry, version int) ([]piEntry, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	byID := make(map[string]int, len(entries))
	for i := range entries {
		if entries[i].Timestamp == "" {
			return nil, fmt.Errorf("Pi entry %d has no timestamp", i+1)
		}
		if _, err := time.Parse(time.RFC3339Nano, entries[i].Timestamp); err != nil {
			return nil, fmt.Errorf("Pi entry %q timestamp is invalid: %w", entries[i].ID, err)
		}
		if version < 2 {
			entries[i].ID = fmt.Sprintf("legacy-%d", i)
			if i == 0 {
				entries[i].ParentID = json.RawMessage("null")
			} else {
				entries[i].ParentID = json.RawMessage(fmt.Sprintf("%q", entries[i-1].ID))
			}
		}
		if entries[i].ID == "" {
			return nil, fmt.Errorf("Pi entry %d has no id", i+1)
		}
		if _, exists := byID[entries[i].ID]; exists {
			return nil, fmt.Errorf("Pi entry %q has a duplicate id", entries[i].ID)
		}
		byID[entries[i].ID] = i
	}
	if version < 2 {
		for i := range entries {
			if entries[i].Type != "compaction" || entries[i].FirstKeptIndex == nil {
				continue
			}
			index := *entries[i].FirstKeptIndex
			// The upstream v1 migration indexes the full file entry list,
			// including the session header at index zero.
			if index <= 0 {
				continue
			}
			entryIndex := index - 1
			if entryIndex >= i {
				return nil, fmt.Errorf("Pi compaction entry %q has invalid firstKeptEntryIndex %d", entries[i].ID, index)
			}
			entries[i].FirstKeptEntryID = entries[entryIndex].ID
		}
	}

	parents := make([]string, len(entries))
	for i, entry := range entries {
		parent, err := piParentID(entry.ParentID)
		if err != nil {
			return nil, fmt.Errorf("Pi entry %q: %w", entry.ID, err)
		}
		parents[i] = parent
		if parent != "" {
			if parentIndex, ok := byID[parent]; !ok || parentIndex >= i {
				return nil, fmt.Errorf("Pi entry %q points to missing or future parent %q", entry.ID, parent)
			}
		}
	}

	path := make([]piEntry, 0, len(entries))
	seen := make(map[string]bool)
	current := len(entries) - 1
	for current >= 0 {
		entry := entries[current]
		if seen[entry.ID] {
			return nil, fmt.Errorf("Pi branch contains a parent cycle at %q", entry.ID)
		}
		seen[entry.ID] = true
		path = append(path, entry)
		parent := parents[current]
		if parent == "" {
			break
		}
		current = byID[parent]
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, nil
}

func piParentID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var parent string
	if err := json.Unmarshal(raw, &parent); err != nil || parent == "" {
		return "", fmt.Errorf("parentId must be a string or null")
	}
	return parent, nil
}

// Pi's context builder places the latest compaction summary first, then keeps
// the range beginning at firstKeptEntryId and all entries after the compaction.
func piContextPath(path []piEntry) []piEntry {
	compaction := -1
	for i, entry := range path {
		if entry.Type == "compaction" {
			compaction = i
		}
	}
	if compaction < 0 {
		return path
	}
	selected := []piEntry{path[compaction]}
	found := false
	keepID := path[compaction].FirstKeptEntryID
	for i := 0; i < compaction; i++ {
		entry := path[i]
		if entry.ID == keepID {
			found = true
		}
		if found && !(entry.Type == "message" && piMessageRole(entry.Message) == "system") {
			if entry.Type == "compaction" {
				continue
			}
			selected = append(selected, entry)
		}
	}
	selected = append(selected, path[compaction+1:]...)
	return selected
}

func piMessageRole(raw json.RawMessage) string {
	var message piMessage
	if json.Unmarshal(raw, &message) == nil {
		return message.Role
	}
	return ""
}

func piEntryMessages(entry piEntry) ([]Message, error) {
	switch entry.Type {
	case "message":
		var message piMessage
		if err := json.Unmarshal(entry.Message, &message); err != nil {
			return nil, fmt.Errorf("Pi entry %q has invalid message: %w", entry.ID, err)
		}
		timestamp := entry.Timestamp
		switch message.Role {
		case "user", "assistant":
			parts, err := piContentParts(message.Content)
			if err != nil {
				return nil, fmt.Errorf("Pi entry %q: %w", entry.ID, err)
			}
			if len(parts) == 0 {
				return nil, nil
			}
			for _, part := range parts {
				if message.Role == "user" && part.Type == "tool_call" {
					return nil, fmt.Errorf("Pi entry %q has a tool call in a user message", entry.ID)
				}
				if message.Role == "assistant" && part.Type == "image" {
					return nil, fmt.Errorf("Pi entry %q has an image in an assistant message", entry.ID)
				}
			}
			return []Message{{Role: message.Role, Timestamp: timestamp, Parts: parts}}, nil
		case "toolResult":
			if message.ToolCallID == "" {
				return nil, fmt.Errorf("Pi entry %q tool result has no toolCallId", entry.ID)
			}
			rawContent := bytes.Clone(message.Content)
			if piToolResultTextOnly(message.Content) {
				rawContent = nil
			}
			return []Message{{Role: "tool", Timestamp: timestamp, Parts: []ContentBlock{{Type: "tool_result", ToolResult: &ToolResult{
				ID: message.ToolCallID, Content: piToolResultText(message.Content), IsError: message.IsError, RawContent: rawContent,
			}}}}}, nil
		case "system":
			// Pi's system, custom, and bash messages are provider-specific context.
			// Their exact records remain in Original, but the common model has no
			// honest representation for them.
			return nil, nil
		case "custom", "bashExecution":
			return nil, fmt.Errorf("Pi entry %q message role %q cannot be represented", entry.ID, message.Role)
		default:
			return nil, fmt.Errorf("Pi entry %q message role %q cannot be represented", entry.ID, message.Role)
		}
	case "custom_message", "context_edit":
		return nil, fmt.Errorf("Pi entry %q type %q cannot be represented", entry.ID, entry.Type)
	case "compaction", "branch_summary":
		if entry.Summary == "" {
			return nil, nil
		}
		return []Message{{Role: "summary", Timestamp: entry.Timestamp, Parts: []ContentBlock{{Type: "text", Text: entry.Summary}}}}, nil
	default:
		// Model changes, labels, usage, and extension state do not form common
		// conversation messages. They are retained byte-for-byte by Original.
		return nil, nil
	}
}

func piToolResultText(raw json.RawMessage) string {
	if isJSONNull(raw) {
		return "null"
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &items) == nil {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			if item.Type == "text" {
				parts = append(parts, item.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return text(raw)
}

func piToolResultTextOnly(raw json.RawMessage) bool {
	var items []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return false
	}
	for _, item := range items {
		if item.Type != "text" {
			return false
		}
	}
	return true
}

func piContentParts(raw json.RawMessage) ([]ContentBlock, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var content string
	if json.Unmarshal(raw, &content) == nil {
		return []ContentBlock{{Type: "text", Text: content, Raw: bytes.Clone(raw)}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("message content must be a string or array: %w", err)
	}
	parts := make([]ContentBlock, 0, len(items))
	for _, item := range items {
		var block struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(item, &block); err != nil || block.Type == "" {
			parts = append(parts, ContentBlock{Type: "unknown", Raw: bytes.Clone(item)})
			continue
		}
		switch block.Type {
		case "text":
			parts = append(parts, ContentBlock{Type: "text", Text: block.Text, Raw: bytes.Clone(item)})
		case "image":
			parts = append(parts, ContentBlock{Type: "image", Raw: bytes.Clone(item)})
		case "toolCall":
			parts = append(parts, ContentBlock{Type: "tool_call", Raw: bytes.Clone(item), ToolCall: &ToolCall{ID: block.ID, Name: block.Name, Input: bytes.Clone(block.Arguments)}})
		case "thinking":
			// Thinking is deliberately absent from the common model, as in the
			// other provider readers. Original retains the exact block.
		default:
			parts = append(parts, ContentBlock{Type: "unknown", Raw: bytes.Clone(item)})
		}
	}
	return parts, nil
}

// Write emits a single linear Pi branch. The common model does not carry
// branch IDs, so preserving multiple branches would be dishonest.
func (Pi) Write(w io.Writer, s Session) error {
	if s.NativeID == "" {
		return fmt.Errorf("Pi native ID is required")
	}
	if !piValidSessionID(s.NativeID) {
		return fmt.Errorf("Pi native ID contains unsupported characters")
	}
	headerTimestamp := sessionTimestamp(s)
	if headerTimestamp == "" {
		return fmt.Errorf("Pi session timestamp is required")
	}
	if _, err := time.Parse(time.RFC3339Nano, headerTimestamp); err != nil {
		return fmt.Errorf("Pi session timestamp is invalid: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(piHeader{Type: "session", Version: 3, ID: s.NativeID, Timestamp: headerTimestamp, CWD: s.Workspace}); err != nil {
		return err
	}
	parentID := ""
	firstEntryID := ""
	entryIndex := 0
	toolNames := make(map[string]string)
	lastWasSummary := false
	for i, message := range s.Messages {
		timestamp := message.Timestamp
		if timestamp == "" {
			timestamp = headerTimestamp
		}
		if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
			return fmt.Errorf("message %d timestamp is invalid: %w", i, err)
		}
		if message.Role == "summary" {
			if lastWasSummary {
				return fmt.Errorf("message %d: consecutive summaries cannot be represented by Pi", i)
			}
			if firstEntryID != "" {
				return fmt.Errorf("message %d: summary after messages cannot be represented without reordering context", i)
			}
			if err := validateSummary(message); err != nil {
				return fmt.Errorf("message %d: %w", i, err)
			}
			if len(message.Parts) != 1 {
				return fmt.Errorf("message %d: summary must contain exactly one text part", i)
			}
			for j, part := range message.Parts {
				entryIndex++
				entryID := piWriteID(s.NativeID, entryIndex, "summary")
				firstKept := firstEntryID
				if firstKept == "" {
					firstKept = entryID
				}
				entry := map[string]any{"type": "compaction", "id": entryID, "parentId": parentIDOrNull(parentID), "timestamp": timestamp, "summary": part.Text, "tokensBefore": 0, "firstKeptEntryId": firstKept}
				if err := enc.Encode(entry); err != nil {
					return fmt.Errorf("message %d part %d: %w", i, j, err)
				}
				parentID = entryID
				if firstEntryID == "" {
					firstEntryID = entryID
				}
			}
			lastWasSummary = true
			continue
		}
		lastWasSummary = false
		if message.Role != "user" && message.Role != "assistant" && message.Role != "tool" {
			return fmt.Errorf("message %d: unsupported role %q", i, message.Role)
		}
		if len(message.Parts) == 0 {
			return fmt.Errorf("message %d: empty message", i)
		}
		// Claude keeps tool results in user-role records; Pi can only express
		// them as separate toolResult entries, so split them out first.
		results := message.Parts
		visible := message.Parts
		if message.Role == "user" {
			results = nil
			visible = make([]ContentBlock, 0, len(message.Parts))
			for _, part := range message.Parts {
				if part.Type == "tool_result" {
					results = append(results, part)
					continue
				}
				visible = append(visible, part)
			}
		}
		writeResult := func(j int, part ContentBlock) error {
			if part.Type != "tool_result" || part.ToolResult == nil || part.ToolResult.ID == "" {
				return fmt.Errorf("message %d part %d: tool messages require tool results", i, j)
			}
			if err := piValidateToolResult(part.ToolResult); err != nil {
				return fmt.Errorf("message %d part %d: %w", i, j, err)
			}
			entryIndex++
			entryID := piWriteID(s.NativeID, entryIndex, "tool")
			toolName, ok := toolNames[part.ToolResult.ID]
			if !ok {
				return fmt.Errorf("message %d part %d: tool result %q has no preceding tool call", i, j, part.ToolResult.ID)
			}
			entry := map[string]any{"type": "message", "id": entryID, "parentId": parentIDOrNull(parentID), "timestamp": timestamp, "message": map[string]any{
				"role": "toolResult", "toolCallId": part.ToolResult.ID, "toolName": toolName, "content": piToolResultContent(part.ToolResult), "isError": part.ToolResult.IsError, "timestamp": piTimestampMillis(timestamp),
			}}
			if err := enc.Encode(entry); err != nil {
				return fmt.Errorf("message %d part %d: %w", i, j, err)
			}
			parentID = entryID
			if firstEntryID == "" {
				firstEntryID = entryID
			}
			return nil
		}
		if message.Role == "tool" {
			for j, part := range results {
				if err := writeResult(j, part); err != nil {
					return err
				}
			}
			continue
		}
		if message.Role == "user" && len(results) > 0 {
			for j, part := range results {
				if err := writeResult(j, part); err != nil {
					return err
				}
			}
			if len(visible) == 0 {
				continue
			}
		}
		content := make([]any, 0, len(visible))
		for j, part := range visible {
			if message.Role == "user" && part.Type == "tool_call" {
				return fmt.Errorf("message %d part %d: Pi user messages cannot contain tool calls", i, j)
			}
			if message.Role == "assistant" && (part.Type == "image" || part.Type == "input_image") {
				return fmt.Errorf("message %d part %d: Pi assistant messages cannot contain images", i, j)
			}
			value, err := piWriteContent(part)
			if err != nil {
				return fmt.Errorf("message %d part %d: %w", i, j, err)
			}
			content = append(content, value)
		}
		entryIndex++
		entryID := piWriteID(s.NativeID, entryIndex, message.Role)
		entry := map[string]any{"type": "message", "id": entryID, "parentId": parentIDOrNull(parentID), "timestamp": timestamp, "message": map[string]any{
			"role": message.Role, "content": content, "timestamp": piTimestampMillis(timestamp),
		}}
		if err := enc.Encode(entry); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
		parentID = entryID
		if firstEntryID == "" {
			firstEntryID = entryID
		}
		for _, part := range message.Parts {
			if part.Type == "tool_call" && part.ToolCall != nil {
				toolNames[part.ToolCall.ID] = qualifiedToolName(part.ToolCall)
			}
		}
	}
	_, err := io.Copy(w, &buf)
	return err
}

func piValidSessionID(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || (i > 0 && (r == '.' || r == '_' || r == '-'))) {
			return false
		}
	}
	last := value[len(value)-1]
	return (last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') || (last >= '0' && last <= '9')
}

func parentIDOrNull(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func piWriteID(sessionID string, index int, role string) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", sessionID, index, role)))
	return fmt.Sprintf("%x", hash[:4])
}

func piTimestampMillis(timestamp string) int64 {
	parsed, _ := time.Parse(time.RFC3339Nano, timestamp)
	return parsed.UnixMilli()
}

func piWriteContent(part ContentBlock) (any, error) {
	switch part.Type {
	case "text":
		return map[string]any{"type": "text", "text": part.Text}, nil
	case "tool_call":
		if part.ToolCall == nil || part.ToolCall.ID == "" || part.ToolCall.Name == "" {
			return nil, fmt.Errorf("invalid tool call")
		}
		if part.ToolCall.Status != "" {
			return nil, fmt.Errorf("tool call status %q cannot be represented by Pi", part.ToolCall.Status)
		}
		input, ok := decodeObjectInput(part.ToolCall.Input)
		if !ok {
			return nil, fmt.Errorf("tool input must be a JSON object")
		}
		var arguments map[string]json.RawMessage
		if err := json.Unmarshal(input, &arguments); err != nil || arguments == nil {
			return nil, fmt.Errorf("tool input must be a JSON object")
		}
		return map[string]any{"type": "toolCall", "id": part.ToolCall.ID, "name": qualifiedToolName(part.ToolCall), "arguments": arguments}, nil
	case "image", "input_image":
		return piImageForBlock(part)
	default:
		return nil, fmt.Errorf("unsupported content %q", part.Type)
	}
}

func piValidateToolResult(result *ToolResult) error {
	if result.IsError && result.ID == "" {
		return fmt.Errorf("tool result has no id")
	}
	if len(result.RawContent) > 0 {
		if _, err := piToolResultBlocks(result.RawContent); err == nil {
			return nil
		}
		if _, err := toolResultText(result); err != nil {
			return err
		}
	}
	return nil
}

func piToolResultContent(result *ToolResult) any {
	if blocks, err := piToolResultBlocks(result.RawContent); err == nil && blocks != nil {
		return blocks
	}
	content, err := toolResultText(result)
	if err != nil {
		content = result.Content
	}
	return []any{map[string]any{"type": "text", "text": content}}
}

func piToolResultBlocks(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, fmt.Errorf("no native content")
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, fmt.Errorf("tool result content is not an array")
	}
	blocks := make([]any, 0, len(items))
	for _, item := range items {
		var block struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MIMEType string `json:"mimeType"`
		}
		if json.Unmarshal(item, &block) != nil || (block.Type != "text" && block.Type != "image") {
			return nil, fmt.Errorf("tool result contains unsupported content")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil {
			return nil, fmt.Errorf("tool result content is not an object")
		}
		allowed := []string{"type", "text"}
		if block.Type == "image" {
			allowed = []string{"type", "data", "mimeType"}
		}
		if err := rejectUnexpectedFields(fields, allowed...); err != nil {
			return nil, err
		}
		if block.Type == "image" && (block.Data == "" || block.MIMEType == "") {
			return nil, fmt.Errorf("tool result image is incomplete")
		}
		if block.Type == "image" {
			if _, err := base64.StdEncoding.DecodeString(block.Data); err != nil {
				return nil, fmt.Errorf("tool result image data is invalid: %w", err)
			}
		}
		value := map[string]any{"type": block.Type}
		if block.Type == "text" {
			value["text"] = block.Text
		} else {
			value["data"] = block.Data
			value["mimeType"] = block.MIMEType
		}
		blocks = append(blocks, value)
	}
	return blocks, nil
}

func piImageForBlock(part ContentBlock) (any, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(part.Raw, &raw); err != nil || raw == nil {
		return nil, fmt.Errorf("image block is not a JSON object")
	}
	if part.Type == "image" {
		var data, mimeType string
		if json.Unmarshal(raw["data"], &data) == nil && json.Unmarshal(raw["mimeType"], &mimeType) == nil && data != "" && mimeType != "" {
			if _, err := base64.StdEncoding.DecodeString(data); err != nil {
				return nil, fmt.Errorf("Pi image data is invalid: %w", err)
			}
			if err := rejectUnexpectedFields(raw, "type", "data", "mimeType"); err != nil {
				return nil, err
			}
			return map[string]any{"type": "image", "data": data, "mimeType": mimeType}, nil
		}
		var source map[string]json.RawMessage
		if json.Unmarshal(raw["source"], &source) == nil && source != nil {
			var kind, mimeType, data string
			if json.Unmarshal(source["type"], &kind) == nil && kind == "base64" && json.Unmarshal(source["media_type"], &mimeType) == nil && json.Unmarshal(source["data"], &data) == nil && data != "" {
				if err := rejectUnexpectedFields(raw, "type", "source"); err != nil {
					return nil, err
				}
				if err := rejectUnexpectedFields(source, "type", "media_type", "data"); err != nil {
					return nil, err
				}
				if mimeType == "" {
					return nil, fmt.Errorf("image media type is empty")
				}
				if _, err := base64.StdEncoding.DecodeString(data); err != nil {
					return nil, fmt.Errorf("image data is invalid: %w", err)
				}
				return map[string]any{"type": "image", "data": data, "mimeType": mimeType}, nil
			}
		}
	}
	if part.Type == "input_image" {
		var imageURL string
		if json.Unmarshal(raw["image_url"], &imageURL) == nil && strings.HasPrefix(imageURL, "data:") {
			if err := rejectUnexpectedFields(raw, "type", "image_url"); err != nil {
				return nil, err
			}
			mimeType, data, err := decodeDataURL(imageURL)
			if err != nil {
				return nil, err
			}
			return map[string]any{"type": "image", "data": data, "mimeType": mimeType}, nil
		}
	}
	return nil, fmt.Errorf("image type %q cannot be represented by Pi", part.Type)
}

// piContentIsEmpty reports message content that produces no common parts:
// absent or null content, an empty array, or an empty string.
func piContentIsEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 || isJSONNull(raw) {
		return true
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		return len(items) == 0
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value == ""
	}
	return false
}
