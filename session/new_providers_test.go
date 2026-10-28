// These tests cover the Qoder, DeepSeek Harness, Qwen Code, and Copilot CLI
// providers. Qoder has local real-session coverage; the others are checked
// against the documented format shapes.
package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

var qoderFixture = []byte(`{"id":"8c13d9d9-2561-45c3-b224-97534cccd8ed","session_id":"123e4567-e89b-12d3-a456-426614174000","parent_id":"","role":"user","parts":[{"type":"text","data":{"type":"text","text":"run the check"}}],"model":"","provider":"","created_at":1761199733377,"updated_at":1761199733377,"finished_at":1761199733377,"is_meta":true}
{"id":"9b713379-4ba1-42ce-a2e9-7ec0261343ba","session_id":"123e4567-e89b-12d3-a456-426614174000","parent_id":"8c13d9d9-2561-45c3-b224-97534cccd8ed","role":"user","parts":[{"type":"text","data":{"type":"text","text":"run the check now"}}],"model":"","provider":"","created_at":1761199733378,"updated_at":1761199733378,"finished_at":1761199733378,"is_meta":false}
{"id":"aa000000-0000-4000-8000-000000000001","session_id":"123e4567-e89b-12d3-a456-426614174000","parent_id":"9b713379-4ba1-42ce-a2e9-7ec0261343ba","role":"assistant","parts":[{"type":"tool_call","data":{"type":"function","id":"call_fc7b","name":"Glob","input":"{\"pattern\":\"**/*.sql\"}","finished":true}},{"type":"finish","data":{"reason":"tool_use","time":1761199992635}}],"model":"qoder-model","provider":"","created_at":1761199992635,"updated_at":1761199992635,"finished_at":1761199992635,"is_meta":false}
{"id":"aa000000-0000-4000-8000-000000000002","session_id":"123e4567-e89b-12d3-a456-426614174000","parent_id":"aa000000-0000-4000-8000-000000000001","role":"tool","parts":[{"type":"tool_result","data":{"tool_use_id":"call_fc7b","content":"No files found.","metadata":{},"is_error":true}}],"model":"","provider":"","created_at":1761199993000,"updated_at":1761199993000,"finished_at":1761199993000,"is_meta":false}
`)

func TestQoderReadsRealShapes(t *testing.T) {
	s := parseProviderTemp(t, "qoder", qoderFixture)
	if s.NativeID != "123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("unexpected native id %q", s.NativeID)
	}
	// The meta message is dropped; the remaining three keep their chain.
	if len(s.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(s.Messages), s.Messages)
	}
	call := s.Messages[1].Parts[0].ToolCall
	if call == nil || call.ID != "call_fc7b" || call.Name != "Glob" || string(call.Input) != `{"pattern":"**/*.sql"}` {
		t.Fatalf("tool call misread: %+v", call)
	}
	result := s.Messages[2].Parts[0].ToolResult
	if result == nil || result.ID != "call_fc7b" || result.Content != "No files found." || !result.IsError {
		t.Fatalf("tool result misread: %+v", result)
	}
	limitations := skippedQoderLimitations(s)
	if len(limitations) == 0 || !strings.Contains(limitations[0], "1 Qoder meta message") {
		t.Fatalf("meta message was not reported: %v", limitations)
	}
}

func TestQoderWriteReadRoundTrip(t *testing.T) {
	s, err := (Qoder{}).Read(bytes.NewReader(qoderFixture))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (Qoder{}).Write(&out, s); err != nil {
		t.Fatal(err)
	}
	got, err := (Qoder{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := projectedParts(s)
	projectionSource := s
	if _, err := compareProjections(projectionSource, got, true); err != nil {
		t.Fatalf("projection changed: %v", err)
	}
	_ = want
	if got.NativeID != s.NativeID || len(got.Messages) != len(s.Messages) {
		t.Fatalf("round trip lost content: %+v", got.Messages)
	}
}

func TestQoderBranchChainFailsClosed(t *testing.T) {
	branch := bytes.Replace(qoderFixture, []byte(`"parent_id":"9b713379-4ba1-42ce-a2e9-7ec0261343ba"`), []byte(`"parent_id":"ffff0000-0000-4000-8000-000000000009"`), 1)
	if _, err := (Qoder{}).Read(bytes.NewReader(branch)); err == nil || !strings.Contains(err.Error(), "branches") {
		t.Fatalf("branch chain was accepted: %v", err)
	}
}

func TestQoderConvertToClaudeKeepsToolError(t *testing.T) {
	s, err := parseQoderFromTemp(t, qoderFixture)
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (Claude{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range target.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.ToolResult != nil && part.ToolResult.IsError {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("claude target lost the qoder tool error: %+v", target.Messages)
	}
}

func parseProviderTemp(t *testing.T, source string, data []byte) Session {
	t.Helper()
	path := filepath.Join(t.TempDir(), "123e4567-e89b-12d3-a456-426614174000.jsonl")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if source == "qoder" {
		if err := os.WriteFile(strings.TrimSuffix(path, ".jsonl")+"-session.json", []byte(`{"id":"123e4567-e89b-12d3-a456-426614174000","working_dir":"/work"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Parse(path, source)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func parseQoderFromTemp(t *testing.T, data []byte) (Session, error) {
	s := parseProviderTemp(t, "qoder", data)
	return s, nil
}

func TestQoderParseUsesSidecarWorkspace(t *testing.T) {
	s, err := parseQoderFromTemp(t, qoderFixture)
	if err != nil {
		t.Fatal(err)
	}
	if s.Workspace != "/work" {
		t.Fatalf("workspace = %q, want /work", s.Workspace)
	}
	var restored bytes.Buffer
	if err := s.RestoreOriginal(&restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Bytes(), qoderFixture) {
		t.Fatal("qoder original bytes changed")
	}
}

var deepSeekFixture = []byte(`{"id":"123e4567-e89b-12d3-a456-426614174000","createdAt":"2026-01-01T00:00:00Z","cwd":"/work"}
{"type":"user/message","seq":1,"time":"2026-01-01T00:00:01Z","data":{"source":{"kind":"user"},"content":"list files"}}
{"type":"assistant/message","seq":2,"time":"2026-01-01T00:00:02Z","data":{"message":{"content":[{"type":"text","text":"listing"},{"type":"reasoning","text":"SECRET"}]}}}
{"type":"tool/call","seq":3,"time":"2026-01-01T00:00:03Z","data":{"name":"bash","callId":"c1","arguments":{"command":"ls"}}}
{"type":"tool/result","seq":4,"time":"2026-01-01T00:00:04Z","data":{"message":{"source":{"callId":"c1"},"isError":false,"content":[{"type":"text","text":"a.txt"}]}}}
`)

func TestDeepSeekReadsEventsAndDeltas(t *testing.T) {
	var stream []byte
	stream = append(stream, deepSeekFixture...)
	stream = append(stream, []byte(`{"type":"assistant/chunk","seq":5,"time":"2026-01-01T00:00:05Z","data":{"chunk":{"type":"text-delta","text":"partial "}}}`+"\n")...)
	stream = append(stream, []byte(`{"type":"assistant/chunk","seq":6,"time":"2026-01-01T00:00:06Z","data":{"chunk":{"type":"text-delta","text":"thought"}}}`+"\n")...)
	stream = append(stream, []byte(`{"type":"turn/end","seq":7,"time":"2026-01-01T00:00:07Z","data":{}}`+"\n")...)
	s, err := (DeepSeek{}).Read(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant text (reasoning dropped), tool call, tool result, flushed deltas.
	if len(s.Messages) != 5 {
		t.Fatalf("messages = %d, want 5: %+v", len(s.Messages), s.Messages)
	}
	if s.Messages[1].Parts[0].Text != "listing" {
		t.Fatalf("assistant text misread: %+v", s.Messages[1])
	}
	last := s.Messages[4].Parts[0].Text
	if last != "partial thought" {
		t.Fatalf("deltas were not assembled: %q", last)
	}
	withOriginal := parseProviderTemp(t, "deepseek", stream)
	limitations := skippedDeepSeekLimitations(withOriginal)
	if len(limitations) == 0 || !strings.Contains(limitations[0], "1 dsh reasoning block") {
		t.Fatalf("reasoning was not reported: %v", limitations)
	}
}

func TestDeepSeekRoundTrip(t *testing.T) {
	s, err := (DeepSeek{}).Read(bytes.NewReader(deepSeekFixture))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (DeepSeek{}).Write(&out, s); err != nil {
		t.Fatal(err)
	}
	got, err := (DeepSeek{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 4 || got.NativeID != s.NativeID || got.Workspace != "/work" {
		t.Fatalf("round trip changed the session: %+v", got.Messages)
	}
	if err := compareStrict(s, got); err != nil {
		t.Fatalf("projection changed: %v", err)
	}
}

func compareStrict(source, target Session) error {
	_, err := compareProjections(source, target, false)
	return err
}

func TestDeepSeekZstdFileRestoresExactly(t *testing.T) {
	var compressed bytes.Buffer
	writer, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(deepSeekFixture); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.jsonl.zstd")
	if err := os.WriteFile(path, compressed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Parse(path, "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	var restored bytes.Buffer
	if err := s.RestoreOriginal(&restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Bytes(), compressed.Bytes()) {
		t.Fatal("zstd original bytes changed")
	}
	if len(s.Messages) != 4 {
		t.Fatalf("compressed parse lost messages: %+v", s.Messages)
	}
}

func TestDeepSeekConvertFromClaude(t *testing.T) {
	original := []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}
{"type":"assistant","sessionId":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"a.txt"}]}}
`)
	s, err := (Claude{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (DeepSeek{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Messages) != 3 || target.Messages[2].Parts[0].ToolResult == nil {
		t.Fatalf("deepseek target lost content: %+v", target.Messages)
	}
}

var qwenFixture = []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","parts":[{"text":"read it"}]}}
{"type":"assistant","sessionId":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-01-01T00:00:01Z","message":{"role":"model","parts":[{"text":"SECRET-THOUGHT","thought":true},{"functionCall":{"id":"c1","name":"read_file","args":{"file_path":"a.txt"}}}]}}
{"type":"tool_result","sessionId":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"contents","error":null}}}]}}
{"type":"system","sessionId":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-01-01T00:00:03Z","subtype":"chat_compression","message":{"role":"user","parts":[{"text":"compressed history"}]}}
`)

func TestQwenReadsPartsAndReportsDrops(t *testing.T) {
	s := parseProviderTemp(t, "qwen", qwenFixture)
	if len(s.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(s.Messages), s.Messages)
	}
	call := s.Messages[1].Parts[0].ToolCall
	if call == nil || call.Name != "read_file" || string(call.Input) != `{"file_path":"a.txt"}` {
		t.Fatalf("functionCall misread: %+v", call)
	}
	result := s.Messages[2].Parts[0].ToolResult
	if result == nil || result.Content != "contents" || result.IsError {
		t.Fatalf("functionResponse misread: %+v", result)
	}
	joined := strings.Join(skippedQwenLimitations(s), "\n")
	if !strings.Contains(joined, "1 Qwen thought block") || !strings.Contains(joined, "compression record") {
		t.Fatalf("qwen drops were not reported: %v", skippedQwenLimitations(s))
	}
}

func TestQwenRoundTrip(t *testing.T) {
	s, err := (Qwen{}).Read(bytes.NewReader(qwenFixture))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (Qwen{}).Write(&out, s); err != nil {
		t.Fatal(err)
	}
	got, err := (Qwen{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 || got.NativeID != s.NativeID {
		t.Fatalf("round trip changed the session: %+v", got.Messages)
	}
	if err := compareStrict(s, got); err != nil {
		t.Fatalf("projection changed: %v", err)
	}
	if !strings.Contains(out.String(), `"functionResponse"`) {
		t.Fatal("qwen writer did not emit a functionResponse record")
	}
}

func TestQwenConvertToClaudePreservesResult(t *testing.T) {
	s := parseProviderTemp(t, "qwen", qwenFixture)
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (Claude{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range target.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.ToolResult != nil && part.ToolResult.ID == "c1" && part.ToolResult.Content == "contents" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("claude target lost the qwen tool result: %+v", target.Messages)
	}
}

var copilotFixture = []byte(`{"type":"session.start","timestamp":"2026-01-01T00:00:00Z","data":{"sessionId":"123e4567-e89b-12d3-a456-426614174000","startTime":"2026-01-01T00:00:00Z","context":{"cwd":"/work"}}}
{"type":"user.message","timestamp":"2026-01-01T00:00:01Z","data":{"content":"<skill-context source=\"x\">SECRET</skill-context>please look"}}
{"type":"assistant.message","timestamp":"2026-01-01T00:00:02Z","data":{"content":"checking"}}
{"type":"tool.execution_start","timestamp":"2026-01-01T00:00:03Z","data":{"toolName":"bash","toolCallId":"c1","arguments":{"command":"ls"}}}
{"type":"tool.execution_complete","timestamp":"2026-01-01T00:00:04Z","data":{"toolCallId":"c1","result":{"content":"a.txt"}}}
{"type":"session.compaction_complete","timestamp":"2026-01-01T00:00:05Z","data":{"success":true,"summaryContent":"earlier work summarized"}}
{"type":"session.shutdown","timestamp":"2026-01-01T00:00:06Z","data":{}}
`)

func TestCopilotReadsEventsAndStripsSkillContext(t *testing.T) {
	s := parseProviderTemp(t, "copilot", copilotFixture)
	if s.NativeID != "123e4567-e89b-12d3-a456-426614174000" || s.Workspace != "/work" {
		t.Fatalf("identity misread: %q %q", s.NativeID, s.Workspace)
	}
	if len(s.Messages) != 5 {
		t.Fatalf("messages = %d, want 5: %+v", len(s.Messages), s.Messages)
	}
	if s.Messages[0].Parts[0].Text != "please look" {
		t.Fatalf("skill context was not stripped: %q", s.Messages[0].Parts[0].Text)
	}
	if s.Messages[4].Role != "summary" || s.Messages[4].Parts[0].Text != "earlier work summarized" {
		t.Fatalf("compaction summary misread: %+v", s.Messages[4])
	}
	joined := strings.Join(skippedCopilotLimitations(s), "\n")
	if !strings.Contains(joined, "1 Copilot skill-context block") {
		t.Fatalf("skill context was not reported: %v", skippedCopilotLimitations(s))
	}
}

func TestCopilotRoundTrip(t *testing.T) {
	s, err := (Copilot{}).Read(bytes.NewReader(copilotFixture))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (Copilot{}).Write(&out, s); err != nil {
		t.Fatal(err)
	}
	got, err := (Copilot{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 5 || got.NativeID != s.NativeID || got.Workspace != "/work" {
		t.Fatalf("round trip changed the session: %+v", got.Messages)
	}
	if err := compareStrict(s, got); err != nil {
		t.Fatalf("projection changed: %v", err)
	}
}

func TestCopilotConvertToCodexReportsErrorFlagLoss(t *testing.T) {
	original := []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"failed","is_error":true}]}}
{"type":"assistant","sessionId":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"ls"}}]}}
`)
	s, err := (Claude{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "copilot")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "flag is dropped") {
		t.Fatalf("copilot error flag loss was not reported: %v", transfer.Limitations)
	}
}

// dsh tool-result fields live inside data.message; the writer must emit
// the same shape.
func TestDeepSeekToolResultMessageShape(t *testing.T) {
	s := parseProviderTemp(t, "deepseek", deepSeekFixture)
	var out bytes.Buffer
	if err := (DeepSeek{}).Write(&out, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"source":{"callId":"c1"}`) || !strings.Contains(out.String(), `"type":"tool/result"`) {
		t.Fatalf("writer did not emit the native tool/result shape: %s", out.String())
	}
}

// Deltas buffered when the next speaker arrives are a complete interrupted
// turn; they must land before the new event, not after it.
func TestDeepSeekDeltasFlushBeforeNextSpeaker(t *testing.T) {
	stream := append([]byte{}, deepSeekFixture...)
	stream = append(stream, []byte(`{"type":"assistant/chunk","seq":5,"time":"2026-01-01T00:00:05Z","data":{"chunk":{"type":"text-delta","text":"interrupted "}}}`+"\n")...)
	stream = append(stream, []byte(`{"type":"assistant/chunk","seq":6,"time":"2026-01-01T00:00:06Z","data":{"chunk":{"type":"text-delta","text":"answer"}}}`+"\n")...)
	stream = append(stream, []byte(`{"type":"user/message","seq":7,"time":"2026-01-01T00:00:07Z","data":{"source":{"kind":"user"},"content":"next question"}}`+"\n")...)
	s, err := (DeepSeek{}).Read(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 6 {
		t.Fatalf("messages = %d, want 6: %+v", len(s.Messages), s.Messages)
	}
	if s.Messages[4].Parts[0].Text != "interrupted answer" || s.Messages[5].Parts[0].Text != "next question" {
		t.Fatalf("delta flush order wrong: %+v", s.Messages[4:])
	}
}

// A v4 assistant message repeats its tool call as a block; the block is
// superseded by the tool/call event and reported, not fatal.
func TestDeepSeekSupersededToolCallBlockIsReported(t *testing.T) {
	data := append([]byte{}, deepSeekFixture...)
	data = append(data, []byte(`{"type":"assistant/message","seq":9,"time":"2026-01-01T00:00:09Z","data":{"message":{"content":[{"type":"tool-call","callId":"c1"},{"type":"text","text":"done"}]}}}`+"\n")...)
	s := parseProviderTemp(t, "deepseek", data)
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatalf("superseded tool-call block failed the conversion: %v", err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "superseded by their tool/call events") {
		t.Fatalf("superseded block was not reported: %v", transfer.Limitations)
	}
}

// Copilot apply_patch calls carry freeform patch strings; they must read
// cleanly and be reported as wrapped for object-only targets.
func TestCopilotApplyPatchStringArguments(t *testing.T) {
	data := `{"type":"session.start","timestamp":"2026-01-01T00:00:00Z","data":{"sessionId":"123e4567-e89b-12d3-a456-426614174000","startTime":"2026-01-01T00:00:00Z","context":{"cwd":"/work"}}}
{"type":"user.message","timestamp":"2026-01-01T00:00:01Z","data":{"content":"patch it"}}
{"type":"tool.execution_start","timestamp":"2026-01-01T00:00:02Z","data":{"toolName":"apply_patch","toolCallId":"c1","arguments":"*** Begin Patch"}}
{"type":"tool.execution_complete","timestamp":"2026-01-01T00:00:03Z","data":{"toolCallId":"c1","result":{"content":"patched"}}}
`
	s := parseProviderTemp(t, "copilot", []byte(data))
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "free-text input") {
		t.Fatalf("patch-string arguments were not reported: %v", transfer.Limitations)
	}
}

// A qwen failure with no output keeps its error text as the result content.
func TestQwenErrorOnlyResponseKeepsReason(t *testing.T) {
	data := strings.Replace(string(qwenFixture),
		`{"output":"contents","error":null}`,
		`{"output":null,"error":"Command failed with exit code 127"}`, 1)
	s := parseProviderTemp(t, "qwen", []byte(data))
	result := s.Messages[2].Parts[0].ToolResult
	if result == nil || !result.IsError || !strings.Contains(result.Content, "exit code 127") {
		t.Fatalf("error reason was lost: %+v", result)
	}
}
