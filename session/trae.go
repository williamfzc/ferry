package session

// This file implements the JSONL rollout written by the TRAE CLI (the
// `traecli`/`traex` command, version 2.x).  TRAE CLI is a closed-source fork
// of Codex CLI: its rollout envelope and response_item records are compatible
// with Codex, while its live transcript also contains history_mutation and
// item_completed records.  The reader accepts both dialects and always keeps
// the source bytes in Session.Original.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Trae is the TRAE CLI session codec.  It deliberately does not claim to
// support the encrypted IDE database or the pre-2.0 CoCo event store: those
// are different products and formats.
type Trae struct{}

type traeRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type traeMeta struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
}

type traeHistory struct {
	Operation          string                  `json:"operation"`
	TurnID             string                  `json:"turn_id"`
	NumTurns           int                     `json:"num_turns"`
	Items              []json.RawMessage       `json:"items"`
	DisplayCompletions []traeDisplayCompletion `json:"display_completions"`
}

type traeDisplayCompletion struct {
	TurnID string          `json:"turn_id"`
	Item   json.RawMessage `json:"item"`
}

type traeItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	CallID    string          `json:"call_id"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Content   json.RawMessage `json:"content"`
	Status    string          `json:"status"`
	IsError   bool            `json:"is_error"`
}

func traeTurnID(raw json.RawMessage) string {
	var value struct {
		TurnID string `json:"turn_id"`
		Meta   struct {
			TurnID string `json:"turn_id"`
		} `json:"internal_chat_message_metadata_passthrough"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	if value.TurnID != "" {
		return value.TurnID
	}
	return value.Meta.TurnID
}

type traeEvent struct {
	Type    string          `json:"type"`
	Message string          `json:"message"`
	Item    json.RawMessage `json:"item"`
	TurnID  string          `json:"turn_id"`
}

// Read parses a TRAE CLI rollout and preserves every input byte.  Incomplete
// final lines are ignored because the CLI appends rollouts while a turn is in
// progress; a malformed line in the middle of a file is still an error.
func (Trae) Read(r io.Reader) (Session, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Session{SourceType: "trae"}, err
	}
	s, err := readTrae(data)
	if err != nil {
		return s, err
	}
	s.Original = bytes.Clone(data)
	return s, nil
}

func readTrae(data []byte) (Session, error) {
	s := Session{SourceType: "trae"}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return s, fmt.Errorf("empty Trae session")
	}

	// Keep canonical history when it is available.  item_completed is a UI
	// notification and normally repeats the same message, so using both would
	// duplicate prompts and answers.  Older rollouts without history_mutation
	// fall back to response_item and event_msg records below.
	var parsedItems []traeParsed
	rolledBackTurns := make(map[string]bool)
	metaSeen := false
	recognized := false
	var last time.Time
	lines := bytes.Split(data, []byte{'\n'})
	for lineNo, raw := range lines {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var rec traeRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			// A live rollout can end with a torn, unterminated JSON line.  Keep
			// it in Original, but do not make an otherwise usable session fail.
			if lineNo == len(lines)-1 && !bytes.HasSuffix(data, []byte{'\n'}) {
				break
			}
			return s, fmt.Errorf("Trae session:%d: %w", lineNo+1, err)
		}
		if rec.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil && t.After(last) {
				last = t
			}
		}
		switch rec.Type {
		case "session_meta":
			recognized = true
			if metaSeen {
				continue
			}
			var meta traeMeta
			if err := json.Unmarshal(rec.Payload, &meta); err != nil {
				return s, fmt.Errorf("Trae session:%d: invalid session_meta: %w", lineNo+1, err)
			}
			s.NativeID = meta.ID
			if s.NativeID == "" {
				s.NativeID = meta.SessionID
			}
			s.Workspace = meta.Cwd
			if last.IsZero() && meta.Timestamp != "" {
				last, _ = time.Parse(time.RFC3339Nano, meta.Timestamp)
			}
			metaSeen = true
		case "history_mutation":
			recognized = true
			var history traeHistory
			if err := json.Unmarshal(rec.Payload, &history); err != nil {
				return s, fmt.Errorf("Trae session:%d: invalid history_mutation: %w", lineNo+1, err)
			}
			switch history.Operation {
			case "append":
			case "replace":
				// replace contains the complete current history.  Keeping the
				// earlier append records would resurrect messages that the CLI
				// deliberately replaced (usually after compaction).
				parsedItems = parsedItems[:0]
				clear(rolledBackTurns)
			case "rollback":
				if history.NumTurns > 1 || history.NumTurns < 0 {
					return s, fmt.Errorf("Trae session:%d: unsupported rollback of %d turns", lineNo+1, history.NumTurns)
				}
				// A rollback carries no items; remove only the explicitly named
				// turn.  Refuse an unaddressable rollback instead of returning a
				// stale transcript that claims to include rolled-back work.
				var mutation struct {
					TargetTurnID string `json:"target_turn_id"`
				}
				if err := json.Unmarshal(rec.Payload, &mutation); err != nil || mutation.TargetTurnID == "" {
					return s, fmt.Errorf("Trae session:%d: rollback has no target_turn_id", lineNo+1)
				}
				rolledBackTurns[mutation.TargetTurnID] = true
				filtered := parsedItems[:0]
				for _, parsed := range parsedItems {
					if parsed.turnID != mutation.TargetTurnID {
						filtered = append(filtered, parsed)
					}
				}
				parsedItems = filtered
			default:
				return s, fmt.Errorf("Trae session:%d: unsupported history_mutation operation %q", lineNo+1, history.Operation)
			}
			for _, item := range history.Items {
				if parsed, ok := traeItemMessage(item, rec.Timestamp); ok {
					for i := range parsed {
						if parsed[i].turnID == "" {
							parsed[i].turnID = history.TurnID
						}
					}
					parsedItems = append(parsedItems, parsed...)
				}
			}
			for _, completion := range history.DisplayCompletions {
				if parsed, ok := traeCompletedItem(completion.Item, rec.Timestamp); ok {
					for i := range parsed {
						if parsed[i].turnID == "" {
							parsed[i].turnID = completion.TurnID
						}
					}
					parsedItems = append(parsedItems, parsed...)
				}
			}
		case "response_item":
			recognized = true
			if parsed, ok := traeResponse(rec.Payload, rec.Timestamp); ok {
				parsedItems = append(parsedItems, parsed...)
			}
		case "event_msg":
			recognized = true
			var event traeEvent
			if err := json.Unmarshal(rec.Payload, &event); err != nil {
				return s, fmt.Errorf("Trae session:%d: invalid event_msg: %w", lineNo+1, err)
			}
			if event.Type == "item_completed" {
				if parsed, ok := traeCompletedItem(event.Item, rec.Timestamp); ok {
					for i := range parsed {
						if parsed[i].turnID == "" {
							parsed[i].turnID = event.TurnID
						}
					}
					parsedItems = append(parsedItems, parsed...)
				}
			} else if event.Type == "user_message" || event.Type == "agent_message" {
				role := "user"
				if event.Type == "agent_message" {
					role = "assistant"
				}
				if event.Message != "" {
					parsedItems = append(parsedItems, traeParsed{message: Message{Role: role, Timestamp: rec.Timestamp, Parts: []ContentBlock{{Type: "text", Text: event.Message}}}, turnID: event.TurnID})
				}
			}
		case "compacted":
			recognized = true
			var value struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(rec.Payload, &value) == nil && value.Message != "" {
				parsed := traeParsed{message: Message{Role: "summary", Timestamp: rec.Timestamp, Parts: []ContentBlock{{Type: "text", Text: value.Message}}}}
				parsedItems = append(parsedItems, parsed)
			}
		case "turn_context", "world_state", "token_usage_record", "inter_agent_communication":
			recognized = true
		}
	}
	if !recognized {
		return s, fmt.Errorf("Trae session has no recognizable rollout records")
	}
	if !metaSeen || s.NativeID == "" {
		return s, fmt.Errorf("Trae session has no session_meta identity")
	}
	// Keep the complete record order.  A history_mutation is canonical, but
	// older rollouts can contain useful response/event records beside it.  Only
	// exact, identity-bearing replays are collapsed; equal text without a
	// shared identity is retained because it may be a real repeated turn.
	for _, item := range mergeTraeParsed(parsedItems) {
		if item.turnID != "" && rolledBackTurns[item.turnID] {
			continue
		}
		s.Messages = append(s.Messages, item.message)
	}
	if !last.IsZero() {
		s.Updated = last
	}
	return s, nil
}

type traeParsed struct {
	message  Message
	identity string
	turnID   string
}

func traeItemMessage(raw json.RawMessage, timestamp string) ([]traeParsed, bool) {
	var item traeItem
	if json.Unmarshal(raw, &item) != nil {
		return nil, false
	}
	switch item.Type {
	case "message":
		if item.Role != "user" && item.Role != "assistant" {
			return nil, false
		}
		if item.Role == "user" && traeSyntheticContextMessage(raw) {
			return nil, false
		}
		parts := traeContentParts(item.Content)
		if len(parts) == 0 {
			return nil, false
		}
		return []traeParsed{{message: Message{Role: item.Role, Timestamp: timestamp, Parts: parts}, identity: item.ID, turnID: traeTurnID(raw)}}, true
	case "function_call", "custom_tool_call":
		callID := item.CallID
		if callID == "" {
			callID = item.ID
		}
		if callID == "" || item.Name == "" {
			return nil, false
		}
		input := item.Arguments
		if len(input) == 0 {
			input = item.Input
		}
		call := &ToolCall{ID: callID, Name: item.Name, Namespace: item.Namespace, Input: input, Status: acpToolCallStatus(item.Status)}
		return []traeParsed{{message: Message{Role: "assistant", Timestamp: timestamp, Parts: []ContentBlock{{Type: "tool_call", ToolCall: call, Raw: bytes.Clone(raw)}}}, identity: "call:" + callID, turnID: traeTurnID(raw)}}, true
	case "function_call_output", "custom_tool_call_output":
		if item.CallID == "" {
			return nil, false
		}
		result := &ToolResult{ID: item.CallID, Content: traeToolResultText(item.Output), IsError: item.IsError || traeNestedIsError(item.Output), RawContent: traeStripReasoning(item.Output)}
		return []traeParsed{{message: Message{Role: "tool", Timestamp: timestamp, Parts: []ContentBlock{{Type: "tool_result", ToolResult: result, Raw: bytes.Clone(raw)}}}, identity: "result:" + item.CallID, turnID: traeTurnID(raw)}}, true
	default:
		return nil, false
	}
}

func traeResponse(raw json.RawMessage, timestamp string) ([]traeParsed, bool) {
	var item struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return nil, false
	}
	if item.Type == "message" && item.Role == "user" && traeRuntimeUserMessage(raw) {
		return nil, false
	}
	return traeItemMessage(raw, timestamp)
}

func traeRuntimeUserMessage(raw json.RawMessage) bool {
	return traeSyntheticContextMessage(raw)
}

// traeSyntheticContextMessage recognizes the complete context envelopes that
// Trae injects as user-role records. A marker inside an ordinary user message
// is deliberately not enough: quoted markers stay visible in the projection.
// The source bytes remain in Session.Original and are reported as an omitted
// projection by skippedTraeLimitations.
func traeSyntheticContextMessage(raw json.RawMessage) bool {
	var item struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Meta    struct {
			Kinds []string `json:"content_item_kinds"`
		} `json:"internal_chat_message_metadata_passthrough"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return false
	}
	if item.Role != "user" && item.Type != "UserMessage" {
		return false
	}
	if len(item.Meta.Kinds) > 0 {
		for _, kind := range item.Meta.Kinds {
			switch kind {
			case "agents_md.instructions", "environments.environment_context", "environments.instructions",
				"plugins.usage_instructions", "memories.instructions", "host_skills", "trae_todo.reminder",
				"multi_agent.role_instructions", "multi_agent.mode_instructions", "multi_agent.usage_hint",
				"collaboration_mode.instructions", "permissions.instructions", "compaction.reminder",
				"generic.developer_instructions", "compaction.summary":
			default:
				return false
			}
		}
		return true
	}
	return traeSyntheticContextText(traeToolResultText(item.Content))
}

func traeSyntheticContextText(content string) bool {
	content = strings.TrimSpace(content)
	sameEnvelope := func(open, close string) bool {
		return strings.HasPrefix(content, open) && strings.HasSuffix(content, close)
	}
	switch {
	case sameEnvelope("<environment_context>", "</environment_context>"):
		return true
	case sameEnvelope("<system-reminder>", "</system-reminder>"):
		return true
	case sameEnvelope("<plugins_instructions>", "</plugins_instructions>"):
		return true
	case sameEnvelope("<INSTRUCTIONS>", "</INSTRUCTIONS>"):
		return true
	case strings.HasPrefix(content, "# AGENTS.md instructions") &&
		strings.Contains(content, "<INSTRUCTIONS>") &&
		strings.HasSuffix(content, "</INSTRUCTIONS>"):
		return true
	default:
		return false
	}
}

func traeCompletedItem(raw json.RawMessage, timestamp string) ([]traeParsed, bool) {
	var item struct {
		Type    string          `json:"type"`
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return nil, false
	}
	role := ""
	switch item.Type {
	case "UserMessage":
		role = "user"
	case "AgentMessage":
		role = "assistant"
	default:
		return nil, false
	}
	parts := traeContentParts(item.Content)
	if len(parts) == 0 {
		return nil, false
	}
	return []traeParsed{{message: Message{Role: role, Timestamp: timestamp, Parts: parts}, identity: item.ID, turnID: traeTurnID(raw)}}, true
}

// traeContentParts handles both Codex block spellings and the title-cased
// item_completed blocks used by TRAE CLI's UI event stream.
func traeContentParts(raw json.RawMessage) []ContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var one map[string]json.RawMessage
	if json.Unmarshal(raw, &one) == nil && one != nil {
		return []ContentBlock{traeContentPart(raw)}
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		if value := stringValue(raw); value != "" {
			return []ContentBlock{{Type: "text", Text: value, Raw: bytes.Clone(raw)}}
		}
		return nil
	}
	parts := make([]ContentBlock, 0, len(items))
	for _, item := range items {
		parts = append(parts, traeContentPart(item))
	}
	return compactTraeParts(parts)
}

func traeContentPart(raw json.RawMessage) ContentBlock {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return ContentBlock{Type: "unknown", Raw: bytes.Clone(raw)}
	}
	kind := stringValue(fields["type"])
	// Some Trae records use the generic text block shape for model reasoning
	// and mark it with source:"reasoning". Check the source before treating a
	// text-typed block as transcript content, otherwise private reasoning leaks
	// into the common message projection.
	if traeReasoningContentKind(kind) || strings.EqualFold(stringValue(fields["source"]), "reasoning") {
		return ContentBlock{}
	}
	if kind == "Text" || kind == "input_text" || kind == "output_text" || kind == "text" {
		return ContentBlock{Type: "text", Text: stringValue(fields["text"]), Raw: bytes.Clone(raw)}
	}
	if kind == "input_image" {
		// Trae follows the Codex content shape for user images. Keep the
		// provider-native block, including its exact bytes, so the common
		// image conversion helpers can validate and translate it later.
		var imageURL string
		if json.Unmarshal(fields["image_url"], &imageURL) == nil && imageURL != "" {
			return ContentBlock{Type: "input_image", Raw: bytes.Clone(raw)}
		}
	}
	if kind == "tool_use" || kind == "function_call" || kind == "custom_tool_call" {
		callID := stringValue(fields["id"])
		if callID == "" {
			callID = stringValue(fields["call_id"])
		}
		input := fields["input"]
		if len(input) == 0 {
			input = fields["arguments"]
		}
		call := &ToolCall{ID: callID, Name: stringValue(fields["name"]), Namespace: stringValue(fields["namespace"]), Input: bytes.Clone(input), Status: acpToolCallStatus(stringValue(fields["status"]))}
		return ContentBlock{Type: "tool_call", ToolCall: call, Raw: bytes.Clone(raw)}
	}
	if kind == "tool_result" || kind == "function_call_output" || kind == "custom_tool_call_output" {
		id := stringValue(fields["tool_use_id"])
		if id == "" {
			id = stringValue(fields["call_id"])
		}
		content := fields["content"]
		if len(content) == 0 {
			content = fields["output"]
		}
		return ContentBlock{Type: "tool_result", ToolResult: &ToolResult{ID: id, Content: traeToolResultText(content), IsError: traeNestedIsError(raw), RawContent: traeStripReasoning(content)}, Raw: bytes.Clone(raw)}
	}
	return ContentBlock{Type: "unknown", Raw: bytes.Clone(raw)}
}

// traeReasoningContentKind identifies provider content blocks that carry model
// reasoning rather than transcript text.  Their raw bytes stay in
// Session.Original for restoration and audit reporting, but the reasoning text
// must never become a user-visible common message or an unsupported block that
// a target writer might expose.
func traeReasoningContentKind(kind string) bool {
	switch strings.ToLower(kind) {
	case "thinking", "reasoning", "reasoning_text", "source_reasoning", "source_reasoning_text":
		return true
	default:
		return false
	}
}

// traeToolResultText only joins textual content blocks.  The generic session
// helper treats any object with a text field as display text, which makes
// structured output (for example an image block carrying an alt-text field)
// look like tool output.  Trae's function-call outputs are arrays of typed
// content items, so retain the source bytes but expose just their text in the
// common projection.
// traeStripReasoning removes reasoning items from a structured tool output so
// the JSON text a target writer serializes matches the filtered common
// projection instead of leaking private reasoning into every target. An
// unchanged output keeps its exact bytes, and Session.Original retains all
// source bytes regardless.
func traeStripReasoning(raw json.RawMessage) json.RawMessage {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return bytes.Clone(raw)
	}
	filtered := make([]json.RawMessage, 0, len(items))
	stripped := false
	for _, item := range items {
		if traeItemIsReasoning(item) {
			stripped = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !stripped {
		return bytes.Clone(raw)
	}
	if len(filtered) == 0 {
		return nil
	}
	out, err := json.Marshal(filtered)
	if err != nil {
		return bytes.Clone(raw)
	}
	return out
}

func traeItemIsReasoning(item json.RawMessage) bool {
	var fields struct {
		Type   string `json:"type"`
		Source string `json:"source"`
	}
	if json.Unmarshal(item, &fields) != nil {
		return false
	}
	return traeReasoningContentKind(fields.Type) || strings.EqualFold(fields.Source, "reasoning")
}

// traeRejectIdenticalReplays fails early on two identical occurrences of one
// tool call or result. Trae's history replay collapses such duplicates on
// read-back (mergeTraeParsed), so the projection would silently lose one
// occurrence.
func traeRejectIdenticalReplays(s Session) error {
	seen := make(map[string]struct{})
	for i, message := range s.Messages {
		for j, part := range message.Parts {
			var key string
			switch part.Type {
			case "tool_call":
				if part.ToolCall == nil {
					continue
				}
				key = "call|" + part.ToolCall.ID + "|" + part.ToolCall.Name + "|" + part.ToolCall.Namespace + "|" + string(part.ToolCall.Input) + "|" + part.ToolCall.Status
			case "tool_result":
				if part.ToolResult == nil {
					continue
				}
				key = "result|" + part.ToolResult.ID + "|" + part.ToolResult.Content + "|" + string(part.ToolResult.RawContent)
				if part.ToolResult.IsError {
					key += "|error"
				}
			default:
				continue
			}
			if _, ok := seen[key]; ok {
				kind, id := "call", ""
				if part.ToolCall != nil {
					id = part.ToolCall.ID
				}
				if part.ToolResult != nil {
					kind, id = "result", part.ToolResult.ID
				}
				return fmt.Errorf("message %d part %d: Trae collapses identical replays of tool %s %q, so writing would lose one occurrence", i, j, kind, id)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

func traeToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if isJSONNull(raw) {
		return "null"
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			if value := traeToolResultText(item); value != "" {
				parts = append(parts, value)
			}
		}
		return strings.Join(parts, "\n")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return ""
	}
	kind := stringValue(fields["type"])
	if kind == "text" || kind == "input_text" || kind == "output_text" || kind == "Text" || kind == "" {
		if value := stringValue(fields["text"]); value != "" {
			return value
		}
	}
	for _, key := range []string{"output", "content", "value"} {
		if nested, ok := fields[key]; ok {
			if value := traeToolResultText(nested); value != "" {
				return value
			}
		}
	}
	return ""
}

func traeNestedIsError(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var value bool
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		for _, item := range items {
			if traeNestedIsError(item) {
				return true
			}
		}
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for _, key := range []string{"is_error", "isError"} {
		if value, ok := fields[key]; ok {
			var flag bool
			if json.Unmarshal(value, &flag) == nil && flag {
				return true
			}
		}
	}
	for _, key := range []string{"output", "content", "value"} {
		if nested, ok := fields[key]; ok && traeNestedIsError(nested) {
			return true
		}
	}
	return false
}

func compactTraeParts(parts []ContentBlock) []ContentBlock {
	out := parts[:0]
	for _, part := range parts {
		if part.Type != "" {
			out = append(out, part)
		}
	}
	return out
}

func stringValue(raw json.RawMessage) string {
	var value string
	if len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}

// mergeTraeParsed removes only exact replays carrying the same provider
// identity.  In particular, it never treats equal text as proof of a
// duplicate: repeated prompts and repeated tool calls are valid history.
func mergeTraeParsed(items []traeParsed) []traeParsed {
	result := make([]traeParsed, 0, len(items))
	seen := make(map[string]struct{})
	for _, item := range items {
		if item.identity != "" {
			key := item.identity + "\x00" + traeMessageKey(item.message)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
		}
		result = append(result, item)
	}
	return result
}

func traeMessageKey(message Message) string {
	var b strings.Builder
	b.WriteString(message.Role)
	for _, part := range message.Parts {
		b.WriteByte('|')
		b.WriteString(part.Type)
		switch part.Type {
		case "text":
			b.WriteByte(':')
			b.WriteString(part.Text)
		case "tool_call":
			if part.ToolCall != nil {
				b.WriteByte(':')
				b.WriteString(part.ToolCall.ID)
				b.WriteByte(':')
				b.WriteString(part.ToolCall.Name)
				b.WriteByte(':')
				b.WriteString(part.ToolCall.Namespace)
				b.WriteByte(':')
				b.Write(part.ToolCall.Input)
				b.WriteByte(':')
				b.WriteString(part.ToolCall.Status)
			}
		case "tool_result":
			if part.ToolResult != nil {
				b.WriteByte(':')
				b.WriteString(part.ToolResult.ID)
				b.WriteByte(':')
				b.WriteString(part.ToolResult.Content)
				b.WriteByte(':')
				b.Write(part.ToolResult.RawContent)
				if part.ToolResult.IsError {
					b.WriteString(":error")
				}
			}
		default:
			b.WriteByte(':')
			b.Write(part.Raw)
		}
	}
	return b.String()
}

// Write emits the verified TRAE/Codex rollout subset.  It uses the existing
// Codex writer for content validation and record layout, then adds metadata
// fields observed in native TRAE CLI files.  No bytes are emitted on error.
func (Trae) Write(w io.Writer, s Session) error {
	if err := traeRejectIdenticalReplays(s); err != nil {
		return err
	}
	var encoded bytes.Buffer
	if err := write(&encoded, s, "trae"); err != nil {
		return err
	}
	lines := bytes.Split(encoded.Bytes(), []byte{'\n'})
	if len(lines) == 0 || len(bytes.TrimSpace(lines[0])) == 0 {
		return fmt.Errorf("Trae writer produced no session metadata")
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(lines[0], &rec); err != nil {
		return fmt.Errorf("Trae writer metadata: %w", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec["payload"], &payload); err != nil {
		return fmt.Errorf("Trae writer metadata payload: %w", err)
	}
	for key, value := range map[string]string{"source": "cli", "thread_source": "user", "model_provider": "trae"} {
		encodedValue, _ := json.Marshal(value)
		payload[key] = encodedValue
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	rec["payload"] = rewritten
	first, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	lines[0] = first
	output := bytes.Join(lines, []byte{'\n'})
	n, err := w.Write(output)
	if err == nil && n != len(output) {
		return io.ErrShortWrite
	}
	return err
}
