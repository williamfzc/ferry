package session

// This file implements the JSON/JSONL session format used by Gemini CLI.
// Gemini CLI currently stores an append-only JSONL log.  Older releases wrote
// one ConversationRecord JSON object; the reader accepts both forms.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// Gemini is the Gemini CLI session codec.
type Gemini struct{}

func (Gemini) Read(r io.Reader) (Session, error) {
	var original bytes.Buffer
	tee := io.TeeReader(r, &original)
	s, err := readGemini(tee)
	if err != nil {
		return s, err
	}
	s.Original = original.Bytes()
	return s, nil
}

type geminiMessage struct {
	ID        string           `json:"id"`
	Type      string           `json:"type"`
	Timestamp string           `json:"timestamp"`
	Content   json.RawMessage  `json:"content"`
	ToolCalls []geminiToolCall `json:"toolCalls,omitempty"`
}

type geminiToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
	Result    json.RawMessage `json:"result"`
	Status    string          `json:"status"`
	Timestamp string          `json:"timestamp"`
	// ResultTimestamp is Ferry metadata used when a common-model tool result
	// has a different timestamp from its assistant call. Gemini stores the
	// result inline with the call, so without this field that timestamp would
	// be silently replaced by the call timestamp on read-back.
	ResultTimestamp string `json:"resultTimestamp,omitempty"`
}

type geminiMetadata struct {
	SessionID   string `json:"sessionId"`
	ProjectHash string `json:"projectHash"`
	StartTime   string `json:"startTime"`
	LastUpdated string `json:"lastUpdated"`
}

func readGemini(r io.Reader) (Session, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Session{SourceType: "gemini"}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Session{SourceType: "gemini"}, fmt.Errorf("empty Gemini session")
	}

	// Legacy files are one JSON object containing messages.  Trying this first
	// is safe: a JSONL metadata line has no `messages` field and falls through.
	var legacy struct {
		geminiMetadata
		Messages []geminiMessage `json:"messages"`
	}
	if json.Unmarshal(data, &legacy) == nil && legacy.SessionID != "" && legacy.Messages != nil {
		return finishGemini(legacy.geminiMetadata, legacy.Messages)
	}

	meta := geminiMetadata{}
	messages := make(map[string]geminiMessage)
	order := make([]string, 0)
	sawRecord := false
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 4096), 64*1024*1024)
	line := 0
	for scan.Scan() {
		line++
		if len(bytes.TrimSpace(scan.Bytes())) == 0 {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(scan.Bytes(), &raw); err != nil {
			// Gemini CLI's own loader skips malformed lines, but Ferry must
			// fail here: silently dropping a line would make a conversion look
			// complete while losing source data. Original is only published on
			// a successful read, so callers can safely retry or restore source.
			return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: %w", line, err)
		}
		sawRecord = true
		if id, ok := stringField(raw, "sessionId"); ok {
			meta.SessionID = id
			if v, ok := stringField(raw, "projectHash"); ok {
				meta.ProjectHash = v
			}
			if v, ok := stringField(raw, "startTime"); ok {
				meta.StartTime = v
			}
			if v, ok := stringField(raw, "lastUpdated"); ok {
				meta.LastUpdated = v
			}
			if msgs, ok := raw["messages"]; ok && string(msgs) != "null" {
				var records []geminiMessage
				if err := json.Unmarshal(msgs, &records); err != nil {
					return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: messages: %w", line, err)
				}
				messages = make(map[string]geminiMessage)
				order = order[:0]
				for _, msg := range records {
					if err := putGeminiMessage(messages, &order, msg); err != nil {
						return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: %w", line, err)
					}
				}
			}
			continue
		}
		if set, ok := raw["$set"]; ok {
			var update map[string]json.RawMessage
			if err := json.Unmarshal(set, &update); err != nil || update == nil {
				return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: invalid $set record", line)
			}
			applyGeminiMetadata(&meta, update)
			// The official history service replaces messages only when the
			// value is an array; a null value must not clear the session.
			if msgs, ok := update["messages"]; ok && string(msgs) != "null" {
				var records []geminiMessage
				if err := json.Unmarshal(msgs, &records); err != nil {
					return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: invalid $set.messages: %w", line, err)
				}
				messages = make(map[string]geminiMessage)
				order = order[:0]
				for _, msg := range records {
					if err := putGeminiMessage(messages, &order, msg); err != nil {
						return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: %w", line, err)
					}
				}
			}
			continue
		}
		if patch, ok := raw["$patch"]; ok {
			if err := applyGeminiPatch(messages, &order, patch); err != nil {
				return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: %w", line, err)
			}
			continue
		}
		if _, hasRewind := raw["$rewindTo"]; hasRewind {
			rewind, ok := stringField(raw, "$rewindTo")
			if !ok || rewind == "" {
				return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: invalid $rewindTo record", line)
			}
			if !removeGeminiFrom(messages, &order, rewind) {
				return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: $rewindTo target %q was not found", line, rewind)
			}
			continue
		}
		if id, ok := stringField(raw, "id"); ok {
			var msg geminiMessage
			if err := json.Unmarshal(scan.Bytes(), &msg); err != nil {
				return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: message: %w", line, err)
			}
			msg.ID = id
			if err := putGeminiMessage(messages, &order, msg); err != nil {
				return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: %w", line, err)
			}
			continue
		}
		// Keep forward-compatible non-message records intact at the source
		// boundary, but never silently discard a known Gemini message. A known
		// message without its id cannot be replayed safely.
		if messageType, ok := stringField(raw, "type"); ok && isKnownGeminiMessageType(messageType) {
			return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session:%d: message record has no id", line)
		}
		continue
	}
	if err := scan.Err(); err != nil {
		return Session{SourceType: "gemini"}, err
	}
	if !sawRecord || meta.SessionID == "" {
		return Session{SourceType: "gemini"}, fmt.Errorf("Gemini session has no sessionId metadata")
	}
	ordered := make([]geminiMessage, 0, len(order))
	for _, id := range order {
		if msg, ok := messages[id]; ok {
			ordered = append(ordered, msg)
		}
	}
	return finishGemini(meta, ordered)
}

func finishGemini(meta geminiMetadata, records []geminiMessage) (Session, error) {
	s := Session{SourceType: "gemini", NativeID: meta.SessionID}
	if meta.LastUpdated != "" {
		if t, err := time.Parse(time.RFC3339Nano, meta.LastUpdated); err == nil {
			s.Updated = t
		}
	} else if meta.StartTime != "" {
		if t, err := time.Parse(time.RFC3339Nano, meta.StartTime); err == nil {
			s.Updated = t
		}
	}
	for i, record := range records {
		if record.ID == "" {
			return s, fmt.Errorf("Gemini message %d has no id", i)
		}
		if record.Type != "user" && record.Type != "gemini" {
			continue
		}
		parts, err := geminiParts(record.Content)
		if err != nil {
			return s, fmt.Errorf("Gemini message %q: %w", record.ID, err)
		}
		role := "user"
		if record.Type == "gemini" {
			role = "assistant"
		}
		var results []ContentBlock
		resultTimestamps := make(map[string]string)
		for callIndex, call := range record.ToolCalls {
			if call.ID == "" || call.Name == "" {
				return s, fmt.Errorf("Gemini message %q tool call %d has no id or name", record.ID, callIndex)
			}
			callPart := -1
			for j := range parts {
				if parts[j].Type == "tool_call" && parts[j].ToolCall != nil && parts[j].ToolCall.ID == call.ID {
					callPart = j
					// The official history can expose a functionCall in content
					// as well as the same call in toolCalls. Keep one call.
					if parts[j].ToolCall.Name == "" {
						parts[j].ToolCall.Name = call.Name
					}
					if len(parts[j].ToolCall.Input) == 0 {
						parts[j].ToolCall.Input = call.Args
					}
					if call.Status != "" {
						// An explicit provider status replaces an earlier
						// canonical value. Unknown statuses clear the optional
						// ACP field and remain available in Raw.
						parts[j].ToolCall.Status = acpToolCallStatus(call.Status)
					}
					// Keep the metadata record as the source of status when the
					// same functionCall also appears in content.
					if !geminiHasThoughtSignature(parts[j].Raw) {
						parts[j].Raw = geminiCallRaw(call)
					}
					break
				}
			}
			if callPart == -1 {
				callPart = len(parts)
				parts = append(parts, ContentBlock{Type: "tool_call", Raw: geminiCallRaw(call), ToolCall: &ToolCall{ID: call.ID, Name: call.Name, Input: call.Args, Status: acpToolCallStatus(call.Status)}})
			}
			if call.Status == "error" {
				for j := range parts {
					if parts[j].Type == "tool_result" && parts[j].ToolResult != nil && parts[j].ToolResult.ID == call.ID {
						parts[j].ToolResult.IsError = true
					}
				}
			}
			if len(call.Result) > 0 && !hasGeminiToolResult(parts, call.ID) {
				results = append(results, ContentBlock{Type: "tool_result", ToolResult: &ToolResult{ID: call.ID, Content: geminiRawText(call.Result), IsError: call.Status == "error", RawContent: bytes.Clone(call.Result)}})
				resultTimestamps[call.ID] = call.ResultTimestamp
			}
		}
		// A metadata result belongs to its own tool-role message so every
		// writer can represent it, matching the Codex and OpenCode readers.
		if len(parts) > 0 {
			s.Messages = append(s.Messages, Message{Role: role, Timestamp: record.Timestamp, Parts: parts})
		}
		for _, result := range results {
			// The result timestamp is carried by the tool-call metadata because
			// Gemini has no separate tool-result message. Keep the old fallback
			// for files that predate this Ferry field.
			resultTimestamp := record.Timestamp
			if result.ToolResult != nil && resultTimestamps[result.ToolResult.ID] != "" {
				resultTimestamp = resultTimestamps[result.ToolResult.ID]
			}
			s.Messages = append(s.Messages, Message{Role: "tool", Timestamp: resultTimestamp, Parts: []ContentBlock{result}})
		}
	}
	return s, nil
}

func geminiParts(raw json.RawMessage) ([]ContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return []ContentBlock{{Type: "text", Text: str, Raw: bytes.Clone(raw)}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		var one map[string]json.RawMessage
		if objectErr := json.Unmarshal(raw, &one); objectErr != nil || one == nil {
			return nil, fmt.Errorf("content is neither string, object, nor array: %w", err)
		}
		items = []json.RawMessage{bytes.Clone(raw)}
	}
	parts := make([]ContentBlock, 0, len(items))
	for _, item := range items {
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil || fields == nil {
			parts = append(parts, ContentBlock{Type: "unknown", Raw: bytes.Clone(item)})
			continue
		}
		if textRaw, ok := fields["text"]; ok {
			var value string
			if json.Unmarshal(textRaw, &value) == nil {
				parts = append(parts, ContentBlock{Type: "text", Text: value, Raw: bytes.Clone(item)})
				continue
			}
		}
		if callRaw, ok := fields["functionCall"]; ok {
			var call struct {
				ID   string          `json:"id"`
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			}
			if json.Unmarshal(callRaw, &call) == nil {
				parts = append(parts, ContentBlock{Type: "tool_call", Raw: bytes.Clone(item), ToolCall: &ToolCall{ID: call.ID, Name: call.Name, Input: call.Args}})
				continue
			}
		}
		if resultRaw, ok := fields["functionResponse"]; ok {
			var result struct {
				ID       string          `json:"id"`
				Name     string          `json:"name"`
				Response json.RawMessage `json:"response"`
			}
			if err := json.Unmarshal(resultRaw, &result); err != nil {
				return nil, fmt.Errorf("functionResponse is invalid: %w", err)
			}
			if result.ID == "" || result.Name == "" || result.Response == nil {
				return nil, fmt.Errorf("functionResponse requires id, name, and response")
			}
			var responseObject map[string]any
			if err := json.Unmarshal(result.Response, &responseObject); err != nil || responseObject == nil {
				return nil, fmt.Errorf("functionResponse response must be a JSON object")
			}
			if len(result.Response) > 0 {
				parts = append(parts, ContentBlock{Type: "tool_result", Raw: bytes.Clone(item), ToolResult: &ToolResult{ID: result.ID, Content: geminiRawText(result.Response), RawContent: bytes.Clone(result.Response)}})
				continue
			}
		}
		if inline, ok := fields["inlineData"]; ok {
			var data struct {
				MIME string `json:"mimeType"`
			}
			if json.Unmarshal(inline, &data) == nil && strings.HasPrefix(strings.ToLower(data.MIME), "image/") {
				parts = append(parts, ContentBlock{Type: "image", Raw: bytes.Clone(item)})
				continue
			}
		}
		if file, ok := fields["fileData"]; ok {
			var data struct {
				MIME string `json:"mimeType"`
			}
			if json.Unmarshal(file, &data) == nil && strings.HasPrefix(strings.ToLower(data.MIME), "image/") {
				parts = append(parts, ContentBlock{Type: "image", Raw: bytes.Clone(item)})
				continue
			}
		}
		parts = append(parts, ContentBlock{Type: "unknown", Raw: bytes.Clone(item)})
	}
	return parts, nil
}

func geminiRawText(raw json.RawMessage) string {
	return text(raw)
}

func geminiCallRaw(call geminiToolCall) json.RawMessage {
	raw, err := json.Marshal(call)
	if err != nil {
		return nil
	}
	return raw
}

func hasGeminiToolResult(parts []ContentBlock, id string) bool {
	for _, part := range parts {
		if part.Type == "tool_result" && part.ToolResult != nil && part.ToolResult.ID == id {
			return true
		}
	}
	return false
}

func stringField(raw map[string]json.RawMessage, key string) (string, bool) {
	v, ok := raw[key]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

func applyGeminiMetadata(meta *geminiMetadata, raw map[string]json.RawMessage) {
	if v, ok := stringField(raw, "sessionId"); ok {
		meta.SessionID = v
	}
	if v, ok := stringField(raw, "projectHash"); ok {
		meta.ProjectHash = v
	}
	if v, ok := stringField(raw, "startTime"); ok {
		meta.StartTime = v
	}
	if v, ok := stringField(raw, "lastUpdated"); ok {
		meta.LastUpdated = v
	}
}

func putGeminiMessage(messages map[string]geminiMessage, order *[]string, msg geminiMessage) error {
	if msg.ID == "" {
		if !isKnownGeminiMessageType(msg.Type) {
			return nil
		}
		return fmt.Errorf("Gemini message record has no id")
	}
	if _, exists := messages[msg.ID]; !exists {
		*order = append(*order, msg.ID)
	}
	messages[msg.ID] = msg
	return nil
}

func isKnownGeminiMessageType(messageType string) bool {
	return messageType == "user" || messageType == "gemini"
}

func removeGeminiFrom(messages map[string]geminiMessage, order *[]string, id string) bool {
	for i, current := range *order {
		if current == id {
			for _, remove := range (*order)[i:] {
				delete(messages, remove)
			}
			*order = (*order)[:i]
			return true
		}
	}
	return false
}

func applyGeminiPatch(messages map[string]geminiMessage, order *[]string, raw json.RawMessage) error {
	var patch struct {
		ID        string           `json:"id"`
		Content   json.RawMessage  `json:"content"`
		ToolCalls []geminiToolCall `json:"toolCalls"`
		Updates   []struct {
			ID        string           `json:"id"`
			Content   json.RawMessage  `json:"content"`
			ToolCalls []geminiToolCall `json:"toolCalls"`
		} `json:"updates"`
		RemoveIDs []string `json:"removeIds"`
		OrderIDs  []string `json:"orderIds"`
	}
	if err := json.Unmarshal(raw, &patch); err != nil || string(raw) == "null" {
		return fmt.Errorf("invalid $patch record")
	}
	apply := func(id string, content json.RawMessage, calls []geminiToolCall) {
		msg, ok := messages[id]
		if !ok {
			return
		}
		if content != nil {
			msg.Content = content
		}
		if calls != nil {
			// A Gemini $patch normally carries only {id,result} for a tool
			// call. Merge it into the existing call instead of replacing the
			// name and args that were written with the original message.
			for _, update := range calls {
				found := false
				for i := range msg.ToolCalls {
					if msg.ToolCalls[i].ID != update.ID {
						continue
					}
					if update.Name != "" {
						msg.ToolCalls[i].Name = update.Name
					}
					if update.Args != nil {
						msg.ToolCalls[i].Args = update.Args
					}
					if update.Result != nil {
						msg.ToolCalls[i].Result = update.Result
					}
					if update.Status != "" {
						msg.ToolCalls[i].Status = update.Status
					}
					if update.Timestamp != "" {
						msg.ToolCalls[i].Timestamp = update.Timestamp
					}
					found = true
					break
				}
				if !found {
					msg.ToolCalls = append(msg.ToolCalls, update)
				}
			}
		}
		messages[id] = msg
	}
	if patch.ID != "" {
		if _, ok := messages[patch.ID]; !ok {
			return fmt.Errorf("$patch references nonexistent message %q", patch.ID)
		}
		apply(patch.ID, patch.Content, patch.ToolCalls)
	}
	for _, update := range patch.Updates {
		if update.ID != "" {
			if _, ok := messages[update.ID]; !ok {
				return fmt.Errorf("$patch references nonexistent message %q", update.ID)
			}
			apply(update.ID, update.Content, update.ToolCalls)
		}
	}
	for _, id := range patch.RemoveIDs {
		if _, ok := messages[id]; !ok {
			return fmt.Errorf("$patch references nonexistent message %q", id)
		}
		delete(messages, id)
		for i, current := range *order {
			if current == id {
				*order = append((*order)[:i], (*order)[i+1:]...)
				break
			}
		}
	}
	if len(patch.OrderIDs) > 0 {
		seen := make(map[string]bool)
		next := make([]string, 0, len(*order))
		for _, id := range patch.OrderIDs {
			if _, ok := messages[id]; !ok {
				return fmt.Errorf("$patch references nonexistent message %q", id)
			}
		}
		for _, id := range *order {
			if !containsString(patch.OrderIDs, id) {
				next = append(next, id)
			}
		}
		for _, id := range patch.OrderIDs {
			if _, ok := messages[id]; ok && !seen[id] {
				next = append(next, id)
				seen[id] = true
			}
		}
		*order = next
	}
	return nil
}

func containsString(items []string, needle string) bool {
	for _, item := range items {
		if item == needle {
			return true
		}
	}
	return false
}

func (Gemini) Write(w io.Writer, s Session) error {
	var out bytes.Buffer
	if s.NativeID == "" {
		return fmt.Errorf("Gemini native ID is required")
	}
	if s.Workspace == "" || !filepath.IsAbs(s.Workspace) {
		return fmt.Errorf("Gemini workspace is required to compute projectHash")
	}
	if len(s.Messages) == 0 {
		return fmt.Errorf("Gemini session has no messages")
	}
	start, end, err := geminiSessionTimes(s)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(s.Workspace))
	records := make([]any, 0, len(s.Messages))
	calls := make(map[string]map[string]any)
	for i, msg := range s.Messages {
		if msg.Role == "tool" {
			// Tool-role messages only carry results; merge them into the
			// call records that were emitted earlier.
			if err := geminiMergeResults(msg, calls); err != nil {
				return fmt.Errorf("message %d: %w", i, err)
			}
			continue
		}
		record, err := geminiMessageRecord(msg, s.NativeID, i, calls)
		if err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
		records = append(records, record)
	}
	for _, call := range calls {
		if _, hasResult := call["result"]; hasResult {
			continue
		}
		if _, hasStatus := call["status"]; !hasStatus {
			return fmt.Errorf("tool call %v has neither a result nor a known status; refusing to claim success", call["id"])
		}
	}
	// Gemini 0.33.x, which is still widely installed, accepts the legacy
	// single-object session file but rejects a Ferry-produced JSONL file. The
	// reader remains able to consume both the legacy object and newer JSONL.
	enc := json.NewEncoder(&out)
	if err := enc.Encode(map[string]any{
		"sessionId":   s.NativeID,
		"projectHash": hex.EncodeToString(hash[:]),
		"startTime":   start,
		"lastUpdated": end,
		"messages":    records,
	}); err != nil {
		return err
	}
	_, err = io.Copy(w, &out)
	return err
}

func geminiSessionTimes(s Session) (string, string, error) {
	var first, last time.Time
	for i, msg := range s.Messages {
		if msg.Timestamp == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, msg.Timestamp)
		if err != nil {
			return "", "", fmt.Errorf("message %d timestamp is invalid: %w", i, err)
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
		if last.IsZero() || t.After(last) {
			last = t
		}
	}
	if first.IsZero() {
		if s.Updated.IsZero() {
			return "", "", fmt.Errorf("Gemini session timestamp is required")
		}
		first, last = s.Updated.UTC(), s.Updated.UTC()
	}
	if !s.Updated.IsZero() && s.Updated.After(last) {
		last = s.Updated.UTC()
	}
	return first.UTC().Format(time.RFC3339Nano), last.UTC().Format(time.RFC3339Nano), nil
}

func geminiMessageRecord(msg Message, sessionID string, index int, calls map[string]map[string]any) (map[string]any, error) {
	if msg.Role != "user" && msg.Role != "assistant" {
		return nil, fmt.Errorf("unsupported role %q", msg.Role)
	}
	if len(msg.Parts) == 0 {
		return nil, fmt.Errorf("empty message")
	}
	parts := make([]any, 0, len(msg.Parts))
	messageCalls := make([]any, 0)
	for j, part := range msg.Parts {
		switch part.Type {
		case "text":
			if geminiHasThoughtSignature(part.Raw) {
				return nil, fmt.Errorf("part %d: thought signature cannot be represented without loss", j)
			}
			parts = append(parts, map[string]any{"text": part.Text})
		case "tool_call":
			if msg.Role != "assistant" || part.ToolCall == nil || part.ToolCall.ID == "" || part.ToolCall.Name == "" {
				return nil, fmt.Errorf("part %d: invalid tool call", j)
			}
			args, _, err := wrapToolInputForCall(part)
			if err != nil {
				return nil, fmt.Errorf("part %d: tool input: %w", j, err)
			}
			// Validate the object shape, then keep the exact bytes: routing
			// numbers through float64 would corrupt integers above 2^53.
			var object map[string]json.RawMessage
			if err := json.Unmarshal(args, &object); err != nil || object == nil {
				return nil, fmt.Errorf("part %d: tool input must be an object", j)
			}
			exact, err := exactJSON(args)
			if err != nil {
				return nil, fmt.Errorf("part %d: tool input is invalid JSON: %w", j, err)
			}
			if geminiHasThoughtSignature(part.Raw) {
				return nil, fmt.Errorf("part %d: thought signature cannot be represented without loss", j)
			}
			if _, duplicate := calls[part.ToolCall.ID]; duplicate {
				return nil, fmt.Errorf("part %d: duplicate tool call id %q", j, part.ToolCall.ID)
			}
			callRecord := map[string]any{"id": part.ToolCall.ID, "name": qualifiedToolName(part.ToolCall), "args": json.RawMessage(exact), "timestamp": msg.Timestamp}
			status := geminiToolStatus(part.Raw)
			// Raw blocks from another provider may carry a status with the
			// same meaning but a different vocabulary (Codex uses
			// "completed", while Gemini uses "success"). Prefer the
			// common status whenever the raw value is not Gemini-native.
			if !isGeminiToolStatus(status) {
				if mapped := geminiStatusForACP(part.ToolCall.Status); mapped != "" {
					status = mapped
				}
			}
			switch status {
			case "":
			case "completed":
				callRecord["status"] = "success"
			default:
				if !isGeminiToolStatus(status) {
					return nil, fmt.Errorf("part %d: unsupported tool status %q", j, status)
				}
				callRecord["status"] = status
			}
			calls[part.ToolCall.ID] = callRecord
			messageCalls = append(messageCalls, callRecord)
		case "tool_result":
			if part.ToolResult == nil || part.ToolResult.ID == "" {
				return nil, fmt.Errorf("part %d: invalid tool result", j)
			}
			if msg.Role == "assistant" || geminiFunctionResponseName(part.Raw) == "" {
				if err := geminiAttachResult(calls, part.ToolResult, msg.Timestamp); err != nil {
					return nil, fmt.Errorf("part %d: %w", j, err)
				}
				continue
			}
			if msg.Role == "user" {
				name := geminiFunctionResponseName(part.Raw)
				if name == "" {
					return nil, fmt.Errorf("part %d: function response name is required and is not available in the common model", j)
				}
				if err := validateGeminiFunctionResponseFields(part.Raw); err != nil {
					return nil, fmt.Errorf("part %d: %w", j, err)
				}
				response, err := geminiFunctionResponseObject(part)
				if err != nil {
					return nil, fmt.Errorf("part %d: %w", j, err)
				}
				functionResponse := map[string]any{"id": part.ToolResult.ID, "name": name, "response": response}
				responseParts, hasParts, err := geminiFunctionResponseParts(part.Raw)
				if err != nil {
					return nil, fmt.Errorf("part %d: %w", j, err)
				}
				if hasParts {
					functionResponse["parts"] = responseParts
				}
				parts = append(parts, map[string]any{"functionResponse": functionResponse})
				continue
			}
			return nil, fmt.Errorf("part %d: tool result cannot be represented inline", j)
		case "image":
			if geminiHasThoughtSignature(part.Raw) {
				return nil, fmt.Errorf("part %d: thought signature cannot be represented without loss", j)
			}
			if len(part.Raw) == 0 || !isGeminiImage(part.Raw) {
				return nil, fmt.Errorf("part %d: image block is not a Gemini inlineData/fileData block", j)
			}
			var value any
			if err := json.Unmarshal(part.Raw, &value); err != nil {
				return nil, fmt.Errorf("part %d: invalid image block: %w", j, err)
			}
			parts = append(parts, value)
		default:
			return nil, fmt.Errorf("part %d: unsupported content %q", j, part.Type)
		}
	}
	if msg.Timestamp == "" {
		return nil, fmt.Errorf("timestamp is required")
	}
	id := fmt.Sprintf("ferry-%s-%d", sessionID, index)
	recordType := "user"
	if msg.Role == "assistant" {
		recordType = "gemini"
	}
	record := map[string]any{"id": id, "type": recordType, "timestamp": msg.Timestamp, "content": parts}
	if len(messageCalls) > 0 {
		record["toolCalls"] = messageCalls
	}
	return record, nil
}

// geminiAttachResult fills a call record with its result. The outcome is
// known whenever a result exists, so the status is derived from it.
func geminiAttachResult(calls map[string]map[string]any, result *ToolResult, resultTimestamp string) error {
	call, ok := calls[result.ID]
	if !ok {
		return fmt.Errorf("tool result %q has no earlier call", result.ID)
	}
	if _, duplicate := call["result"]; duplicate {
		return fmt.Errorf("duplicate result for tool call %q", result.ID)
	}
	call["result"] = geminiToolResult(result)
	if resultTimestamp != "" {
		if callTimestamp, _ := call["timestamp"].(string); callTimestamp != resultTimestamp {
			call["resultTimestamp"] = resultTimestamp
		}
	}
	// The result record is the ground truth of the outcome: it wins even when
	// the call itself claimed the opposite, and toolCallStatusLimitations
	// reports the resulting status change instead of leaving it silent.
	if result.IsError {
		call["status"] = "error"
	} else {
		call["status"] = "success"
	}
	return nil
}

func geminiMergeResults(msg Message, calls map[string]map[string]any) error {
	if len(msg.Parts) == 0 {
		return fmt.Errorf("empty message")
	}
	for j, part := range msg.Parts {
		if part.Type == "text" {
			return fmt.Errorf("part %d: tool text needs a tool result", j)
		}
		if part.Type != "tool_result" || part.ToolResult == nil || part.ToolResult.ID == "" {
			return fmt.Errorf("part %d: invalid tool result", j)
		}
		if err := geminiAttachResult(calls, part.ToolResult, msg.Timestamp); err != nil {
			return fmt.Errorf("part %d: %w", j, err)
		}
	}
	return nil
}

func geminiToolResult(result *ToolResult) any {
	if len(result.RawContent) > 0 {
		if exact, err := exactJSON(result.RawContent); err == nil {
			return json.RawMessage(exact)
		}
	}
	return []any{map[string]any{"text": result.Content}}
}

func geminiToolStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var record struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &record) != nil {
		return ""
	}
	return record.Status
}

func isGeminiToolStatus(status string) bool {
	switch status {
	case "validating", "scheduled", "error", "success", "executing", "cancelled", "awaiting_approval":
		return true
	default:
		return false
	}
}

func geminiStatusForACP(status string) string {
	switch status {
	case "pending":
		return "scheduled"
	case "in_progress":
		return "executing"
	case "completed":
		return "success"
	case "failed":
		return "error"
	default:
		return ""
	}
}

func geminiHasThoughtSignature(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	if _, ok := fields["thoughtSignature"]; ok {
		return true
	}
	if _, ok := fields["thought_signature"]; ok {
		return true
	}
	for _, key := range []string{"functionCall", "function_call"} {
		nested, ok := fields[key]
		if !ok {
			continue
		}
		if geminiHasThoughtSignature(nested) {
			return true
		}
	}
	return false
}

func geminiFunctionResponseName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var block struct {
		FunctionResponse struct {
			Name string `json:"name"`
		} `json:"functionResponse"`
	}
	if json.Unmarshal(raw, &block) != nil {
		return ""
	}
	return block.FunctionResponse.Name
}

func geminiFunctionResponseObject(part ContentBlock) (json.RawMessage, error) {
	if part.ToolResult == nil {
		return nil, fmt.Errorf("function response value is required")
	}
	if len(part.ToolResult.RawContent) == 0 {
		return nil, fmt.Errorf("function response object is not available in the common model")
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(part.ToolResult.RawContent, &response); err != nil || response == nil {
		return nil, fmt.Errorf("function response must be a JSON object")
	}
	exact, err := exactJSON(part.ToolResult.RawContent)
	if err != nil {
		return nil, fmt.Errorf("function response is invalid JSON: %w", err)
	}
	return json.RawMessage(exact), nil
}

func geminiFunctionResponseParts(raw json.RawMessage) ([]any, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var block struct {
		FunctionResponse struct {
			Parts json.RawMessage `json:"parts"`
		} `json:"functionResponse"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, false, fmt.Errorf("function response block is invalid")
	}
	if len(block.FunctionResponse.Parts) == 0 || string(block.FunctionResponse.Parts) == "null" {
		return nil, false, nil
	}
	var parts []any
	dec := json.NewDecoder(bytes.NewReader(block.FunctionResponse.Parts))
	dec.UseNumber()
	if err := dec.Decode(&parts); err != nil {
		return nil, false, fmt.Errorf("function response parts must be an array")
	}
	return parts, true, nil
}

func validateGeminiFunctionResponseFields(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("function response fields are not available in the common model")
	}
	var block struct {
		FunctionResponse map[string]json.RawMessage `json:"functionResponse"`
	}
	if err := json.Unmarshal(raw, &block); err != nil || block.FunctionResponse == nil {
		return fmt.Errorf("function response block is invalid")
	}
	for key := range block.FunctionResponse {
		switch key {
		case "id", "name", "response", "parts":
		default:
			return fmt.Errorf("unsupported function response field %q", key)
		}
	}
	return nil
}

func isGeminiImage(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	_, inline := fields["inlineData"]
	_, file := fields["fileData"]
	return inline || file
}
