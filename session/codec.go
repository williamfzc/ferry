// This file exposes provider codecs and writes the supported session subset.
package session

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// Reader decodes a provider's session into the common representation.
type Reader interface {
	Read(io.Reader) (Session, error)
}

// Writer encodes a session in a provider's format. It rejects unsupported content.
type Writer interface {
	Write(io.Writer, Session) error
}

// Codec reads and writes one provider's session format.
type Codec interface {
	Reader
	Writer
}

// Claude and Codex implement the corresponding JSONL session formats.
type Claude struct{}
type Codex struct{}

func (Claude) Read(r io.Reader) (Session, error)  { return readOriginal(r, "claude", "") }
func (Codex) Read(r io.Reader) (Session, error)   { return readOriginal(r, "codex", "") }
func (Claude) Write(w io.Writer, s Session) error { return write(w, s, "claude") }
func (Codex) Write(w io.Writer, s Session) error  { return write(w, s, "codex") }

func write(w io.Writer, s Session, provider string) error {
	// Trae shares the Codex record layout but keeps provider-specific fields
	// such as the tool-result error flag, so the layout is normalized while
	// the provider stays intact.
	layout := provider
	if layout == "trae" {
		layout = "codex"
	}
	// Build before writing so an unsupported block cannot produce partial output.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	emit := func(v any) error { return enc.Encode(v) }
	if s.NativeID == "" {
		return fmt.Errorf("native ID is required")
	}
	if layout == "codex" {
		// Run content checks first so callers still get the useful content
		// error instead of a metadata error when both are invalid.
		if err := validateMessages(s, layout); err != nil {
			return err
		}
		if !isUUID(s.NativeID) {
			return fmt.Errorf("Codex native ID must be a UUID")
		}
		if !filepath.IsAbs(s.Workspace) {
			return fmt.Errorf("Codex workspace must be an absolute path")
		}
		timestamp := sessionTimestamp(s)
		if timestamp == "" {
			return fmt.Errorf("Codex session timestamp is required")
		}
		if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
			return fmt.Errorf("Codex session timestamp is invalid: %w", err)
		}
		payload := map[string]any{
			"id":          s.NativeID,
			"originator":  "ferry",
			"cli_version": "ferry",
		}
		payload["cwd"] = s.Workspace
		meta := map[string]any{"type": "session_meta", "payload": payload}
		meta["timestamp"] = timestamp
		payload["timestamp"] = timestamp
		if err := emit(meta); err != nil {
			return err
		}
	}
	lastTimestamp := ""
	for i, m := range s.Messages {
		if m.Role == "summary" {
			if err := validateSummary(m); err != nil {
				return fmt.Errorf("message %d: %w", i, err)
			}
			if layout == "claude" {
				return fmt.Errorf("message %d: summary cannot be represented by Claude", i)
			}
			// Codex has a native compaction record. Keep each text part as
			// its own record because the common model preserves part order.
			for j, p := range m.Parts {
				if err := emit(map[string]any{
					"type":      "compacted",
					"timestamp": outputTimestamp(s, m.Timestamp, layout),
					"payload":   map[string]any{"message": p.Text},
				}); err != nil {
					return fmt.Errorf("message %d part %d: %w", i, j, err)
				}
			}
			continue
		}
		if m.Role != "user" && m.Role != "assistant" && m.Role != "tool" {
			return fmt.Errorf("message %d: unsupported role %q", i, m.Role)
		}
		for j, p := range m.Parts {
			if p.Type == "text" && m.Role == "tool" {
				return fmt.Errorf("message %d part %d: tool text needs a tool result", i, j)
			}
			if p.Type == "tool_call" && (p.ToolCall == nil || p.ToolCall.ID == "" || p.ToolCall.Name == "" || m.Role != "assistant") {
				return fmt.Errorf("message %d part %d: invalid tool call", i, j)
			}
			if p.Type == "tool_result" && (p.ToolResult == nil || p.ToolResult.ID == "" || (m.Role != "user" && m.Role != "tool")) {
				return fmt.Errorf("message %d part %d: invalid tool result", i, j)
			}
			if p.Type != "text" && p.Type != "tool_call" && p.Type != "tool_result" && !isImageType(p.Type) {
				return fmt.Errorf("message %d part %d: unsupported content %q", i, j, p.Type)
			}
			if isImageType(p.Type) {
				if _, err := imageForProvider(p, layout); err != nil {
					return fmt.Errorf("message %d part %d: unsupported content %q: %w", i, j, p.Type, err)
				}
			}
		}
		if len(m.Parts) == 0 {
			return fmt.Errorf("message %d: empty message", i)
		}
		if layout == "claude" {
			blocks := make([]any, 0, len(m.Parts))
			role := m.Role
			if role == "tool" {
				role = "user"
			}
			for _, p := range m.Parts {
				switch p.Type {
				case "text":
					blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
				case "tool_call":
					input, _, err := wrapToolInputForCall(p)
					if err != nil {
						return fmt.Errorf("message %d: %w", i, err)
					}
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": p.ToolCall.ID, "name": qualifiedToolName(p.ToolCall), "input": input})
				case "tool_result":
					content, err := toolResultText(p.ToolResult)
					if err != nil {
						return fmt.Errorf("message %d: %w", i, err)
					}
					block := map[string]any{"type": "tool_result", "tool_use_id": p.ToolResult.ID, "content": content}
					if p.ToolResult.IsError {
						block["is_error"] = true
					}
					blocks = append(blocks, block)
				case "image", "input_image":
					image, err := imageForProvider(p, "claude")
					if err != nil {
						return fmt.Errorf("message %d: %w", i, err)
					}
					var value any
					if err := json.Unmarshal(image, &value); err != nil {
						return fmt.Errorf("message %d: invalid image block: %w", i, err)
					}
					blocks = append(blocks, value)
				}
			}
			if err := emit(map[string]any{"type": role, "sessionId": s.NativeID, "cwd": s.Workspace, "timestamp": m.Timestamp, "message": map[string]any{"role": role, "content": blocks}}); err != nil {
				return err
			}
		} else {
			// A missing Codex timestamp is carried forward from the previous
			// message rather than reset to the session start.
			timestamp := outputTimestamp(s, m.Timestamp, provider)
			if m.Timestamp != "" {
				lastTimestamp = m.Timestamp
			} else if lastTimestamp != "" {
				timestamp = lastTimestamp
			}
			for _, p := range m.Parts {
				var payload any
				switch p.Type {
				case "text":
					kind := "input_text"
					if m.Role == "assistant" {
						kind = "output_text"
					}
					payload = map[string]any{"type": "message", "role": m.Role, "content": []any{map[string]any{"type": kind, "text": p.Text}}}
				case "tool_call":
					if isCustomToolCall(p) {
						freeText, err := customToolInput(p.ToolCall.Input)
						if err != nil {
							return fmt.Errorf("message %d: %w", i, err)
						}
						payload = map[string]any{"type": "custom_tool_call", "call_id": p.ToolCall.ID, "name": p.ToolCall.Name, "input": freeText}
						if p.ToolCall.Status != "" {
							if !isACPToolCallStatus(p.ToolCall.Status) {
								return fmt.Errorf("message %d: unsupported tool call status %q", i, p.ToolCall.Status)
							}
							payload.(map[string]any)["status"] = p.ToolCall.Status
						}
						break
					}
					if object, ok := decodeObjectInput(p.ToolCall.Input); ok {
						call := map[string]any{"type": "function_call", "call_id": p.ToolCall.ID, "name": p.ToolCall.Name, "arguments": string(object)}
						if p.ToolCall.Namespace != "" {
							call["namespace"] = p.ToolCall.Namespace
						}
						if p.ToolCall.Status != "" {
							if !isACPToolCallStatus(p.ToolCall.Status) {
								return fmt.Errorf("message %d: unsupported tool call status %q", i, p.ToolCall.Status)
							}
							call["status"] = p.ToolCall.Status
						}
						payload = call
						break
					}
					var freeText string
					if len(p.ToolCall.Input) > 0 && json.Unmarshal(p.ToolCall.Input, &freeText) == nil {
						payload = map[string]any{"type": "custom_tool_call", "call_id": p.ToolCall.ID, "name": p.ToolCall.Name, "input": freeText}
						if p.ToolCall.Status != "" {
							if !isACPToolCallStatus(p.ToolCall.Status) {
								return fmt.Errorf("message %d: unsupported tool call status %q", i, p.ToolCall.Status)
							}
							payload.(map[string]any)["status"] = p.ToolCall.Status
						}
						break
					}
					return fmt.Errorf("message %d: tool input must be a JSON object or free text", i)
				case "tool_result":
					output, err := toolResultText(p.ToolResult)
					if err != nil {
						return fmt.Errorf("message %d: %w", i, err)
					}
					payload = map[string]any{"type": "function_call_output", "call_id": p.ToolResult.ID, "output": output}
					// The observed Codex rollout format has no error flag on
					// function_call_output, so the flag is reported as lost
					// instead of being invented. Trae's reader honors it.
					if p.ToolResult.IsError && provider == "trae" {
						payload.(map[string]any)["is_error"] = true
					}
				case "image", "input_image":
					image, err := imageForProvider(p, "codex")
					if err != nil {
						return fmt.Errorf("message %d: %w", i, err)
					}
					var block any
					if err := json.Unmarshal(image, &block); err != nil {
						return fmt.Errorf("message %d: invalid image block: %w", i, err)
					}
					payload = map[string]any{"type": "message", "role": m.Role, "content": []any{block}}
				}
				if err := emit(map[string]any{"type": "response_item", "timestamp": timestamp, "payload": payload}); err != nil {
					return err
				}
			}
		}
	}
	_, err := io.Copy(w, &buf)
	return err
}

func validateMessages(s Session, provider string) error {
	for i, m := range s.Messages {
		if m.Role == "summary" {
			if err := validateSummary(m); err != nil {
				return fmt.Errorf("message %d: %w", i, err)
			}
			continue
		}
		if m.Role != "user" && m.Role != "assistant" && m.Role != "tool" {
			return fmt.Errorf("message %d: unsupported role %q", i, m.Role)
		}
		if len(m.Parts) == 0 {
			return fmt.Errorf("message %d: empty message", i)
		}
		for j, p := range m.Parts {
			if p.Type == "text" && m.Role == "tool" {
				return fmt.Errorf("message %d part %d: tool text needs a tool result", i, j)
			}
			if p.Type == "tool_call" && (p.ToolCall == nil || p.ToolCall.ID == "" || p.ToolCall.Name == "" || m.Role != "assistant") {
				return fmt.Errorf("message %d part %d: invalid tool call", i, j)
			}
			if p.Type == "tool_result" && (p.ToolResult == nil || p.ToolResult.ID == "" || (m.Role != "user" && m.Role != "tool")) {
				return fmt.Errorf("message %d part %d: invalid tool result", i, j)
			}
			if p.Type != "text" && p.Type != "tool_call" && p.Type != "tool_result" && !isImageType(p.Type) {
				return fmt.Errorf("message %d part %d: unsupported content %q", i, j, p.Type)
			}
			if isImageType(p.Type) {
				if _, err := imageForProvider(p, provider); err != nil {
					return fmt.Errorf("message %d part %d: unsupported content %q: %w", i, j, p.Type, err)
				}
			}
		}
	}
	return nil
}

func validateSummary(m Message) error {
	if len(m.Parts) == 0 {
		return fmt.Errorf("empty summary")
	}
	for j, p := range m.Parts {
		if p.Type != "text" {
			return fmt.Errorf("summary part %d: unsupported content %q", j, p.Type)
		}
	}
	return nil
}

func outputTimestamp(s Session, timestamp, provider string) string {
	if timestamp != "" || provider != "codex" {
		return timestamp
	}
	return sessionTimestamp(s)
}

func isUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// ferryUUID derives a stable RFC 4122 version 5 UUID from a source identity
// so cross-provider output keeps a reproducible session ID instead of a
// random one.
func ferryUUID(source, id string) string {
	h := sha1.New()
	h.Write([]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}) // UUIDv5 DNS namespace
	h.Write([]byte(source + "\x00" + id))
	sum := h.Sum(nil)[:16]
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, b := range sum {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexDigits[b>>4], hexDigits[b&0x0f])
	}
	return string(out)
}

func sessionTimestamp(s Session) string {
	for _, m := range s.Messages {
		if m.Timestamp != "" {
			return m.Timestamp
		}
	}
	if !s.Updated.IsZero() {
		return s.Updated.UTC().Format(time.RFC3339Nano)
	}
	return ""
}

func isImageType(kind string) bool {
	return kind == "image" || kind == "input_image"
}

// qualifiedToolName renders a tool call's full identity. Codex carries MCP
// calls as namespace plus name; Claude and Gemini spell the same identity as
// one "mcp__server__tool" style name.
func qualifiedToolName(call *ToolCall) string {
	if call.Namespace != "" {
		return call.Namespace + "__" + call.Name
	}
	return call.Name
}

// imageForProvider validates and converts the two image block spellings used
// by the providers. It deliberately rejects fields that have no target
// equivalent instead of quietly dropping image metadata.
func imageForProvider(p ContentBlock, provider string) (json.RawMessage, error) {
	if len(p.Raw) == 0 {
		return nil, fmt.Errorf("%s image has no original block", p.Type)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(p.Raw, &raw); err != nil || raw == nil {
		return nil, fmt.Errorf("%s image block is not a JSON object", p.Type)
	}
	var kind string
	if err := json.Unmarshal(raw["type"], &kind); err != nil || kind != p.Type {
		return nil, fmt.Errorf("%s image block has mismatched type", p.Type)
	}

	switch {
	case provider == "claude" && p.Type == "image":
		if err := validateClaudeImage(raw); err != nil {
			return nil, err
		}
		return json.RawMessage(bytes.Clone(p.Raw)), nil
	case provider == "codex" && p.Type == "input_image":
		if err := validateCodexImage(raw); err != nil {
			return nil, err
		}
		return json.RawMessage(bytes.Clone(p.Raw)), nil
	case provider == "codex" && p.Type == "image":
		return claudeToCodexImage(raw)
	case provider == "claude" && p.Type == "input_image":
		return codexToClaudeImage(raw)
	default:
		return nil, fmt.Errorf("image type %q cannot be written to %s", p.Type, provider)
	}
}

func validateClaudeImage(raw map[string]json.RawMessage) error {
	if err := rejectUnexpectedFields(raw, "type", "source"); err != nil {
		return fmt.Errorf("Claude image: %w", err)
	}
	var source map[string]json.RawMessage
	if err := json.Unmarshal(raw["source"], &source); err != nil || source == nil {
		return fmt.Errorf("Claude image has no source")
	}
	var sourceType string
	if err := json.Unmarshal(source["type"], &sourceType); err != nil {
		return fmt.Errorf("Claude image source has no type")
	}
	switch sourceType {
	case "base64":
		if err := rejectUnexpectedFields(source, "type", "media_type", "data"); err != nil {
			return fmt.Errorf("Claude image source: %w", err)
		}
		var mediaType, data string
		if json.Unmarshal(source["media_type"], &mediaType) != nil || mediaType == "" || json.Unmarshal(source["data"], &data) != nil || data == "" {
			return fmt.Errorf("Claude base64 image source is incomplete")
		}
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			return fmt.Errorf("Claude base64 image source is invalid: %w", err)
		}
	case "url":
		if err := rejectUnexpectedFields(source, "type", "url"); err != nil {
			return fmt.Errorf("Claude image source: %w", err)
		}
		var url string
		if json.Unmarshal(source["url"], &url) != nil || url == "" {
			return fmt.Errorf("Claude URL image source is incomplete")
		}
	default:
		return fmt.Errorf("Claude image source type %q cannot be converted", sourceType)
	}
	return nil
}

func validateCodexImage(raw map[string]json.RawMessage) error {
	if err := rejectUnexpectedFields(raw, "type", "image_url", "detail"); err != nil {
		return fmt.Errorf("Codex image: %w", err)
	}
	var imageURL string
	if json.Unmarshal(raw["image_url"], &imageURL) != nil || imageURL == "" {
		return fmt.Errorf("Codex image has no image_url")
	}
	if strings.HasPrefix(imageURL, "data:") {
		if _, _, err := decodeDataURL(imageURL); err != nil {
			return err
		}
	}
	return nil
}

func claudeToCodexImage(raw map[string]json.RawMessage) (json.RawMessage, error) {
	if err := validateClaudeImage(raw); err != nil {
		return nil, err
	}
	var source map[string]json.RawMessage
	if err := json.Unmarshal(raw["source"], &source); err != nil || source == nil {
		return nil, fmt.Errorf("Claude image has no source")
	}
	var sourceType string
	if json.Unmarshal(source["type"], &sourceType) != nil {
		return nil, fmt.Errorf("Claude image source has no type")
	}
	var imageURL string
	switch sourceType {
	case "base64":
		var mediaType, data string
		if json.Unmarshal(source["media_type"], &mediaType) != nil || mediaType == "" || json.Unmarshal(source["data"], &data) != nil || data == "" {
			return nil, fmt.Errorf("Claude base64 image source is incomplete")
		}
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			return nil, fmt.Errorf("Claude base64 image source is invalid: %w", err)
		}
		imageURL = "data:" + mediaType + ";base64," + data
	case "url":
		if json.Unmarshal(source["url"], &imageURL) != nil || imageURL == "" {
			return nil, fmt.Errorf("Claude URL image source is incomplete")
		}
	default:
		return nil, fmt.Errorf("Claude image source type %q cannot be converted to Codex", sourceType)
	}
	return json.Marshal(map[string]any{"type": "input_image", "image_url": imageURL})
}

func codexToClaudeImage(raw map[string]json.RawMessage) (json.RawMessage, error) {
	if err := validateCodexImage(raw); err != nil {
		return nil, err
	}
	if detail, ok := raw["detail"]; ok && string(detail) != "null" {
		var value string
		if json.Unmarshal(detail, &value) != nil || value != "" {
			return nil, fmt.Errorf("Codex image detail cannot be represented by Claude")
		}
	}
	var imageURL string
	if json.Unmarshal(raw["image_url"], &imageURL) != nil {
		return nil, fmt.Errorf("Codex image has no image_url")
	}
	if strings.HasPrefix(imageURL, "data:") {
		mediaType, data, err := decodeDataURL(imageURL)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}})
	}
	return json.Marshal(map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": imageURL}})
}

func decodeDataURL(value string) (string, string, error) {
	parts := strings.SplitN(value, ",", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "data:") || !strings.HasSuffix(parts[0], ";base64") {
		return "", "", fmt.Errorf("Codex data image URL is not a base64 data URL")
	}
	mediaType := strings.TrimPrefix(strings.TrimSuffix(parts[0], ";base64"), "data:")
	if mediaType == "" || parts[1] == "" {
		return "", "", fmt.Errorf("Codex data image URL is incomplete")
	}
	if _, err := base64.StdEncoding.DecodeString(parts[1]); err != nil {
		return "", "", fmt.Errorf("Codex data image URL is invalid: %w", err)
	}
	return mediaType, parts[1], nil
}

func rejectUnexpectedFields(raw map[string]json.RawMessage, allowed ...string) error {
	for key := range raw {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("field %q cannot be represented", key)
		}
	}
	return nil
}

// toolInput accepts JSON objects and Codex's JSON-encoded argument string.
func toolInput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		raw = json.RawMessage(encoded)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("tool input must be a JSON object")
	}
	return raw, nil
}

// toolResultText renders a tool result as text. Structured results are
// serialized as exact JSON text so object-only providers keep every value;
// Convert reports the serialization.
func toolResultText(result *ToolResult) (string, error) {
	if len(result.RawContent) == 0 {
		return result.Content, nil
	}
	var plain string
	if err := json.Unmarshal(result.RawContent, &plain); err == nil && !isJSONNull(result.RawContent) {
		return plain, nil
	}
	// Gemini wraps a plain text result as [{"text": "..."}]; unwrap that one
	// shape so it compares equal to the same text from any provider.
	var wrapper []map[string]json.RawMessage
	if err := json.Unmarshal(result.RawContent, &wrapper); err == nil && len(wrapper) == 1 {
		value, ok := wrapper[0]["text"]
		if ok && len(wrapper[0]) == 1 && json.Unmarshal(value, &plain) == nil {
			return plain, nil
		}
	}
	exact, err := exactJSON(result.RawContent)
	if err != nil {
		return "", fmt.Errorf("structured tool result is invalid JSON: %w", err)
	}
	return string(exact), nil
}

// decodeObjectInput returns a tool call's arguments as a JSON object,
// accepting an object directly or a string whose content is an object (the
// Codex "arguments" convention). Free text reports false.
func decodeObjectInput(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), true
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		decoded := json.RawMessage(encoded)
		var object map[string]json.RawMessage
		if json.Unmarshal(decoded, &object) == nil && object != nil {
			return decoded, true
		}
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		return raw, true
	}
	return nil, false
}

// wrapToolInput adapts ordinary tool-call arguments for providers that require
// a JSON object. Source-aware callers should use wrapToolInputForCall so Codex
// custom-call text is not mistaken for an object.
func wrapToolInput(raw json.RawMessage) (json.RawMessage, bool, error) {
	if object, ok := decodeObjectInput(raw); ok {
		return object, false, nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil {
		wrapped, err := json.Marshal(map[string]string{"input": encoded})
		if err != nil {
			return nil, false, err
		}
		return json.RawMessage(wrapped), true, nil
	}
	return nil, false, fmt.Errorf("tool input must be a JSON object or free text")
}

// isCustomToolCall identifies a Codex custom call from the source payload,
// rather than guessing from its input. Custom calls accept arbitrary text, so
// text that happens to contain a JSON object must remain text.
func isCustomToolCall(p ContentBlock) bool {
	if len(p.Raw) == 0 {
		return false
	}
	var payload struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(p.Raw, &payload) == nil && payload.Type == "custom_tool_call"
}

// customToolInput decodes the JSON string used by Codex custom calls. An
// omitted input is an empty text input, which is still a valid custom call.
func customToolInput(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", fmt.Errorf("custom tool input must be free text: %w", err)
	}
	return text, nil
}

// wrapToolInputForCall adapts a tool call for providers that require an object.
// Codex custom-call text is always wrapped, including text that looks like a
// JSON object; ordinary function-call arguments keep the normal object logic.
func wrapToolInputForCall(p ContentBlock) (json.RawMessage, bool, error) {
	if isCustomToolCall(p) {
		text, err := customToolInput(p.ToolCall.Input)
		if err != nil {
			return nil, false, err
		}
		wrapped, err := json.Marshal(map[string]string{"input": text})
		if err != nil {
			return nil, false, err
		}
		return json.RawMessage(wrapped), true, nil
	}
	return wrapToolInput(p.ToolCall.Input)
}

// exactJSON re-encodes a JSON value with json.Number so integers beyond
// float64 precision (2^53) survive instead of being silently rounded.
func exactJSON(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	// Decode must consume the entire input. A Decoder intentionally permits
	// more than one JSON value, so checking only the first Decode would
	// silently discard trailing data (including malformed data).
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("invalid JSON: multiple values")
		}
		return nil, err
	}
	return json.Marshal(value)
}
