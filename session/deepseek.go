// This file reads and writes DeepSeek Harness (dsh) sessions. A session
// directory holds session.jsonl or a newer session.vN.jsonl generation,
// optionally zstd-framed. Line one is a session header; the remaining lines
// are typed events, including streamed text deltas that must be assembled
// into messages.
package session

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// DeepSeek reads and writes the DeepSeek Harness (dsh) JSONL event format.
type DeepSeek struct{}

func (DeepSeek) Read(r io.Reader) (Session, error) { return readDeepSeek(r) }

func (DeepSeek) Write(w io.Writer, s Session) error { return writeDeepSeek(w, s) }

type deepSeekHeader struct {
	ID            string          `json:"id"`
	CreatedAt     json.RawMessage `json:"createdAt"`
	Cwd           string          `json:"cwd"`
	Origin        string          `json:"origin"`
	ParentSession string          `json:"parentSession"`
}

type deepSeekEvent struct {
	Type  string          `json:"type"`
	Time  json.RawMessage `json:"time"`
	Time0 json.RawMessage `json:"time0"`
	Data  json.RawMessage `json:"data"`
}

// deepSeekTimestamp normalizes the flexible time spellings (RFC 3339 strings
// or epoch numbers in seconds or milliseconds) to one RFC 3339 form so the
// projection compares like with like.
func deepSeekTimestamp(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		return ""
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		if _, err := time.Parse(time.RFC3339Nano, text); err == nil {
			parsed, _ := time.Parse(time.RFC3339Nano, text)
			return parsed.UTC().Format(time.RFC3339Nano)
		}
		return ""
	}
	var number float64
	if json.Unmarshal(trimmed, &number) == nil {
		if number >= 1e12 {
			return time.UnixMilli(int64(number)).UTC().Format(time.RFC3339Nano)
		}
		if number >= 1e9 {
			return time.Unix(int64(number), 0).UTC().Format(time.RFC3339Nano)
		}
	}
	return ""
}

// deepSeekEventTime picks the event timestamp: text-chunks carry time0.
func deepSeekEventTime(event deepSeekEvent) string {
	if event.Type == "text-chunks" && len(event.Time0) > 0 {
		return deepSeekTimestamp(event.Time0)
	}
	return deepSeekTimestamp(event.Time)
}

// readDeepSeek parses one plain dsh JSONL stream (already decompressed).
func readDeepSeek(r io.Reader) (Session, error) {
	s := Session{SourceType: "deepseek"}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 32*1024*1024)
	line := 0
	recognized := false
	haveHeader := false
	var header deepSeekHeader
	pending := make([]string, 0)
	pendingAt := ""
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		s.Messages = append(s.Messages, Message{Role: "assistant", Timestamp: pendingAt, Parts: []ContentBlock{{Type: "text", Text: strings.Join(pending, "")}}})
		pending = make([]string, 0)
		pendingAt = ""
	}
	for scan.Scan() {
		line++
		if !haveHeader {
			if err := json.Unmarshal(scan.Bytes(), &header); err != nil {
				return s, fmt.Errorf("%s:%d: invalid dsh session header: %w", s.SourcePath, line, err)
			}
			if header.ID == "" {
				return s, fmt.Errorf("%s:%d: dsh session header has no id", s.SourcePath, line)
			}
			s.NativeID = strings.TrimPrefix(header.ID, "session-")
			s.Workspace = header.Cwd
			haveHeader = true
			recognized = true
			continue
		}
		var event deepSeekEvent
		if err := json.Unmarshal(scan.Bytes(), &event); err != nil {
			return s, fmt.Errorf("%s:%d: invalid dsh event: %w", s.SourcePath, line, err)
		}
		timestamp := deepSeekEventTime(event)
		recognized = recognized || deepSeekKnownEvent(event.Type)
		switch event.Type {
		case "user/message":
			// Buffered deltas before the next speaker are a complete
			// interrupted turn; flush them so the transcript order stays true.
			flushPending()
			var data struct {
				Source struct {
					Kind string `json:"kind"`
				} `json:"source"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid user/message data: %w", s.SourcePath, line, err)
			}
			if data.Source.Kind != "user" {
				// Plugin-spliced context, not something the user typed.
				continue
			}
			parts := deepSeekContentBlocks(data.Content)
			if len(parts) > 0 {
				s.Messages = append(s.Messages, Message{Role: "user", Timestamp: timestamp, Parts: parts})
			}
		case "assistant/chunk":
			var data struct {
				Chunk struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"chunk"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid assistant/chunk data: %w", s.SourcePath, line, err)
			}
			if data.Chunk.Type == "text-delta" && data.Chunk.Text != "" {
				pending = append(pending, data.Chunk.Text)
				if timestamp != "" {
					pendingAt = timestamp
				}
			}
		case "text-chunks":
			var data struct {
				Texts []json.RawMessage `json:"texts"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid text-chunks data: %w", s.SourcePath, line, err)
			}
			for _, member := range data.Texts {
				var text string
				if json.Unmarshal(member, &text) == nil && text != "" {
					pending = append(pending, text)
				}
			}
			if timestamp != "" {
				pendingAt = timestamp
			}
		case "assistant/message":
			// The persisted final message supersedes the streamed deltas.
			pending = make([]string, 0)
			pendingAt = ""
			var data struct {
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid assistant/message data: %w", s.SourcePath, line, err)
			}
			parts := deepSeekContentBlocks(data.Message.Content)
			if len(parts) > 0 {
				s.Messages = append(s.Messages, Message{Role: "assistant", Timestamp: timestamp, Parts: parts})
			}
		case "step/end", "turn/end":
			flushPending()
		case "tool/call":
			flushPending()
			var data struct {
				Name      string          `json:"name"`
				CallID    string          `json:"callId"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid tool/call data: %w", s.SourcePath, line, err)
			}
			if data.CallID == "" || data.Name == "" {
				return s, fmt.Errorf("%s:%d: dsh tool/call is missing callId or name", s.SourcePath, line)
			}
			input, err := deepSeekArguments(data.Arguments)
			if err != nil {
				return s, fmt.Errorf("%s:%d: %w", s.SourcePath, line, err)
			}
			call := &ToolCall{ID: data.CallID, Name: data.Name, Input: input}
			message := Message{Role: "assistant", Timestamp: timestamp, Parts: []ContentBlock{{Type: "tool_call", ToolCall: call}}}
			s.Messages = append(s.Messages, message)
		case "tool/result":
			flushPending()
			// The call id, error flag, and content live inside data.message,
			// not on the event data itself.
			var data struct {
				Message struct {
					Source struct {
						CallID string `json:"callId"`
					} `json:"source"`
					IsError bool            `json:"isError"`
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid tool/result data: %w", s.SourcePath, line, err)
			}
			if data.Message.Source.CallID == "" {
				return s, fmt.Errorf("%s:%d: dsh tool/result has no message.source.callId", s.SourcePath, line)
			}
			result := &ToolResult{ID: data.Message.Source.CallID, IsError: data.Message.IsError}
			if len(data.Message.Content) > 0 {
				result.Content, result.RawContent, result.IsError = deepSeekResultText(data.Message.Content, result.IsError)
			}
			message := Message{Role: "tool", Timestamp: timestamp, Parts: []ContentBlock{{Type: "tool_result", ToolResult: result}}}
			s.Messages = append(s.Messages, message)
		case "session/title":
			// Session titles have no place in the common model; the original
			// bytes retain them.
		default:
			// Unknown future events stay out of the transcript and are
			// reported by skippedDeepSeekLimitations.
		}
	}
	if err := scan.Err(); err != nil {
		return s, err
	}
	flushPending()
	if !recognized || !haveHeader {
		return s, fmt.Errorf("%s: no recognizable dsh records", s.SourcePath)
	}
	return s, nil
}

func deepSeekKnownEvent(eventType string) bool {
	switch eventType {
	case "session/title", "user/message", "assistant/chunk", "text-chunks", "assistant/message", "step/end", "turn/end", "tool/call", "tool/result":
		return true
	default:
		return false
	}
}

// deepSeekContentBlocks converts one dsh content value (string or block
// array) into common parts. Reasoning blocks are counted, not exposed.
func deepSeekContentBlocks(raw json.RawMessage) []ContentBlock {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		if plain == "" {
			return nil
		}
		return []ContentBlock{{Type: "text", Text: plain, Raw: bytes.Clone(raw)}}
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return []ContentBlock{{Type: "unknown", Raw: bytes.Clone(raw)}}
	}
	parts := make([]ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		var fields struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(block, &fields) != nil {
			parts = append(parts, ContentBlock{Type: "unknown", Raw: bytes.Clone(block)})
			continue
		}
		switch {
		case fields.Type == "text" && fields.Text != "":
			parts = append(parts, ContentBlock{Type: "text", Text: fields.Text, Raw: bytes.Clone(block)})
		case deepSeekToolCallBlockType(fields.Type):
			// A v4 assistant message repeats its tool call as a block; the
			// tool/call event is the record of note. Skip the duplicate
			// (counted by skippedDeepSeekLimitations) instead of failing.
		case fields.Type == "tool-result":
			// Pre-v4 logs nest the tool result inside the message content;
			// its text is transcript content, its errors come with it.
			innerContent, _, _ := deepSeekResultText(fields.Content, false)
			if innerContent != "" {
				parts = append(parts, ContentBlock{Type: "text", Text: innerContent, Raw: bytes.Clone(block)})
			}
		case deepSeekReasoningType(fields.Type):
			// Model reasoning: counted by skippedDeepSeekLimitations, never
			// exposed through the projection.
		default:
			parts = append(parts, ContentBlock{Type: "unknown", Raw: bytes.Clone(block)})
		}
	}
	return parts
}

// deepSeekToolCallBlockType recognizes the assistant-message block forms
// that duplicate a tool/call event.
func deepSeekToolCallBlockType(blockType string) bool {
	switch blockType {
	case "tool-call", "toolCall", "tool_use", "functionCall":
		return true
	default:
		return false
	}
}

func deepSeekReasoningType(blockType string) bool {
	switch strings.ToLower(blockType) {
	case "reasoning", "thinking", "reasoning_text", "thought":
		return true
	default:
		return false
	}
}

type deepSeekResult struct {
	content    string
	rawContent json.RawMessage
	isError    bool
}

// deepSeekResultText joins the text of a dsh tool-result content value and
// reports block-level error flags.
func deepSeekResultText(raw json.RawMessage, isError bool) (string, json.RawMessage, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return "", nil, isError
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain, nil, isError
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return text(raw), bytes.Clone(raw), isError
	}
	joined := make([]string, 0, len(blocks))
	structured := false
	for _, block := range blocks {
		var fields struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"isError"`
		}
		if json.Unmarshal(block, &fields) != nil {
			structured = true
			continue
		}
		if fields.IsError {
			isError = true
		}
		switch {
		case fields.Type == "text" && fields.Text != "":
			joined = append(joined, fields.Text)
		case fields.Type == "tool-result":
			innerContent, _, innerError := deepSeekResultText(fields.Content, false)
			if innerError {
				isError = true
			}
			if innerContent != "" {
				joined = append(joined, innerContent)
			}
		case fields.Type != "" && fields.Type != "text" && !deepSeekReasoningType(fields.Type):
			structured = true
		}
	}
	content := strings.Join(joined, "\n")
	if structured {
		return content, bytes.Clone(raw), isError
	}
	return content, nil, isError
}

// deepSeekArguments accepts a JSON object or an object encoded as a string.
func deepSeekArguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return json.RawMessage(`{}`), nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		if encoded == "" {
			return json.RawMessage(`{}`), nil
		}
		raw = json.RawMessage(encoded)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("dsh tool arguments must be a JSON object")
	}
	return raw, nil
}

// parseDeepSeekFile reads one local dsh log, transparently decoding the
// zstd framing dsh writes by default. The captured original stays the exact
// file bytes so RestoreOriginal reproduces the compressed file.
func parseDeepSeekFile(path string) (Session, error) {
	data, err := os.ReadFile(path)
	s := Session{SourceType: "deepseek", SourcePath: path}
	if err != nil {
		return s, err
	}
	var stream io.Reader = bytes.NewReader(data)
	if strings.HasSuffix(path, ".zstd") {
		decoder, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			s.SourcePath = path
			return s, fmt.Errorf("%s: dsh zstd frame cannot be decoded: %w", path, err)
		}
		defer decoder.Close()
		decompressed, readErr := io.ReadAll(decoder)
		if readErr != nil {
			s.SourcePath = path
			return s, fmt.Errorf("%s: dsh zstd frame cannot be read: %w", path, readErr)
		}
		stream = bytes.NewReader(decompressed)
	}
	s, err = readDeepSeek(stream)
	if err != nil {
		s.SourcePath = path
		return s, err
	}
	s.Original = data
	s.SourcePath = path
	if info, statErr := os.Stat(path); statErr == nil {
		s.Updated = info.ModTime()
	}
	sum := sha256.Sum256([]byte("deepseek\x00" + path))
	s.ID = hex.EncodeToString(sum[:16])
	return s, nil
}

// writeDeepSeek emits the dsh subset: one session header line
// followed by typed events. Streaming deltas are never written; assistant
// text always travels as a persisted assistant/message event.
func writeDeepSeek(w io.Writer, s Session) error {
	if s.NativeID == "" {
		return fmt.Errorf("dsh native ID is required")
	}
	if !isUUID(s.NativeID) {
		return fmt.Errorf("DeepSeek native ID must be a UUID")
	}
	if !filepath.IsAbs(s.Workspace) {
		return fmt.Errorf("DeepSeek workspace must be an absolute path")
	}
	if err := validateMessages(s, "deepseek"); err != nil {
		return err
	}
	headerTimestamp := sessionTimestamp(s)
	if headerTimestamp == "" {
		return fmt.Errorf("DeepSeek session timestamp is required")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	header := map[string]any{"id": s.NativeID, "createdAt": headerTimestamp, "cwd": s.Workspace}
	if err := enc.Encode(header); err != nil {
		return err
	}
	seq := 0
	lastTimestamp := headerTimestamp
	emit := func(eventType string, timestamp string, data map[string]any) error {
		if timestamp == "" {
			timestamp = lastTimestamp
		} else {
			lastTimestamp = timestamp
		}
		seq++
		event := map[string]any{"type": eventType, "seq": seq, "time": timestamp, "data": data}
		return enc.Encode(event)
	}
	for i, message := range s.Messages {
		if message.Role == "summary" {
			return fmt.Errorf("message %d: summary cannot be represented by DeepSeek", i)
		}
		for j, part := range message.Parts {
			switch part.Type {
			case "text":
				if message.Role == "user" {
					if err := emit("user/message", message.Timestamp, map[string]any{"source": map[string]any{"kind": "user"}, "content": part.Text}); err != nil {
						return err
					}
					continue
				}
				if message.Role == "assistant" {
					if err := emit("assistant/message", message.Timestamp, map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": part.Text}}}}); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("message %d part %d: tool text needs a tool result", i, j)
			case "tool_call":
				object, ok := decodeObjectInput(part.ToolCall.Input)
				if !ok {
					return fmt.Errorf("message %d part %d: DeepSeek tool arguments must be a JSON object", i, j)
				}
				data := map[string]any{"name": qualifiedToolName(part.ToolCall), "callId": part.ToolCall.ID, "arguments": json.RawMessage(object)}
				if err := emit("tool/call", message.Timestamp, data); err != nil {
					return err
				}
			case "tool_result":
				content, err := toolResultText(part.ToolResult)
				if err != nil {
					return fmt.Errorf("message %d part %d: %w", i, j, err)
				}
				data := map[string]any{
					"message": map[string]any{
						"source":  map[string]any{"callId": part.ToolResult.ID},
						"isError": part.ToolResult.IsError,
						"content": []any{map[string]any{"type": "text", "text": content}},
					},
				}
				if err := emit("tool/result", message.Timestamp, data); err != nil {
					return err
				}
			default:
				return fmt.Errorf("message %d part %d: unsupported content %q", i, j, part.Type)
			}
		}
	}
	_, err := io.Copy(w, &buf)
	return err
}

// deepSeekLogGeneration classifies a dsh log file name: 0 for
// session.jsonl, N for session.vN.jsonl, -1 when the name is not a dsh log.
// Generation N >= 10 is deliberately accepted so future migrations are not
// silently skipped.
func deepSeekLogGeneration(path string) int {
	name := filepath.Base(path)
	hadZstd := strings.HasSuffix(name, ".zstd")
	if hadZstd {
		name = strings.TrimSuffix(name, ".zstd")
	}
	if name == "session.jsonl" {
		return 0
	}
	if !strings.HasPrefix(name, "session.v") || !strings.HasSuffix(name, ".jsonl") {
		return -1
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, "session.v"), ".jsonl")
	if digits == "" || len(digits) > 9 {
		return -1
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return -1
		}
	}
	if digits[0] == '0' {
		return -1
	}
	generation, err := strconv.Atoi(digits)
	if err != nil {
		return -1
	}
	return generation
}

// discoverDeepSeekFiles walks the dsh sessions root and returns, per
// directory, only the newest generation of each log. dsh migrations leave
// older generations behind, and reading them would resurrect replaced
// history.
func discoverDeepSeekFiles(root string) []string {
	type candidate struct {
		generation int
		modTime    time.Time
		path       string
	}
	byDir := make(map[string]candidate)
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		generation := deepSeekLogGeneration(path)
		if generation < 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		dir := filepath.Dir(path)
		current, seen := byDir[dir]
		if !seen || generation > current.generation ||
			(generation == current.generation && info.ModTime().After(current.modTime)) ||
			(generation == current.generation && info.ModTime().Equal(current.modTime) && path > current.path) {
			byDir[dir] = candidate{generation: generation, modTime: info.ModTime(), path: path}
		}
		return nil
	})
	paths := make([]string, 0, len(byDir))
	for _, candidate := range byDir {
		paths = append(paths, candidate.path)
	}
	sort.Strings(paths)
	return paths
}

// skippedDeepSeekLimitations reports the dsh content that stays out of the
// common projection: model reasoning, unknown future events, and subagent
// runs read as their own sessions.
func skippedDeepSeekLimitations(s Session) []string {
	if s.SourceType != "deepseek" {
		return nil
	}
	reasoning := 0
	superseded := 0
	unknown := make(map[string]int)
	subagent := false
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !bytes.Contains(line, []byte(`"type"`)) && !bytes.Contains(line, []byte(`"id"`)) {
			continue
		}
		var probe struct {
			Type   string `json:"type"`
			Origin string `json:"origin"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		if probe.Origin == "subagent" {
			subagent = true
		}
		if probe.Type == "" {
			continue
		}
		if !deepSeekKnownEvent(probe.Type) {
			name := probe.Type
			unknown[name]++
			continue
		}
		var event deepSeekEvent
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		switch event.Type {
		case "user/message", "assistant/message":
			var data struct {
				Content json.RawMessage `json:"content"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				reasoning += countDeepSeekReasoning(data.Content)
				reasoning += countDeepSeekReasoning(data.Message.Content)
				superseded += countDeepSeekToolCallBlocks(data.Message.Content)
			}
		}
	}
	limitations := make([]string, 0, 3)
	if reasoning > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d dsh reasoning block(s) that have no cross-provider part; the original bytes retain them.", reasoning))
	}
	if superseded > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d dsh tool-call block(s) superseded by their tool/call events; the original bytes retain them.", superseded))
	}
	if subagent {
		limitations = append(limitations, "Source is a dsh subagent run (origin=subagent); the original bytes retain the marker.")
	}
	if len(unknown) > 0 {
		types := make([]string, 0, len(unknown))
		for name := range unknown {
			types = append(types, name)
		}
		sort.Strings(types)
		parts := make([]string, 0, len(types))
		for _, name := range types {
			parts = append(parts, fmt.Sprintf("%s=%d", name, unknown[name]))
		}
		limitations = append(limitations, fmt.Sprintf("Source contains unrepresented dsh event types (%s); the original bytes retain them.", strings.Join(parts, ", ")))
	}
	return limitations
}

func countDeepSeekToolCallBlocks(raw json.RawMessage) int {
	if len(raw) == 0 || isJSONNull(raw) {
		return 0
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return 0
	}
	count := 0
	for _, block := range blocks {
		if deepSeekToolCallBlockType(block.Type) {
			count++
		}
	}
	return count
}

func countDeepSeekReasoning(raw json.RawMessage) int {
	if len(raw) == 0 || isJSONNull(raw) {
		return 0
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return 0
	}
	count := 0
	for _, block := range blocks {
		if deepSeekReasoningType(block.Type) {
			count++
		}
	}
	return count
}
