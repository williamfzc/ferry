// These tests check session parsing and discovery at their public boundary.
package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		source, file string
		messages     int
		content      string
	}{{"claude", "claude.jsonl", 3, "Fix the test"}, {"codex", "codex.jsonl", 3, "Fix the test"}} {
		t.Run(tc.source, func(t *testing.T) {
			s, err := Parse(filepath.Join("..", "testdata", tc.file), tc.source)
			if err != nil {
				t.Fatal(err)
			}
			if s.NativeID != "123e4567-e89b-12d3-a456-426614174010" || len(s.Messages) != tc.messages || s.Messages[0].Parts[0].Text != tc.content {
				t.Fatalf("unexpected session: %+v", s)
			}
			if s.Messages[1].Parts[len(s.Messages[1].Parts)-1].ToolCall == nil {
				t.Fatalf("missing tool call: %+v", s.Messages[1])
			}
			if tc.source == "codex" && (s.Messages[2].Parts[0].ToolResult == nil || s.Messages[2].Parts[0].ToolResult.Content != "passed") {
				t.Fatalf("missing tool output: %+v", s.Messages[2])
			}
		})
	}
}

func TestCodexToolCallStatusUsesACPValuesAndKeepsUnknownRaw(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := `{"type":"session_meta","payload":{"id":"codex-status","cwd":"/work"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call","status":"completed","call_id":"done","name":"read","arguments":"{}"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call","status":"queued_for_retry","call_id":"future","name":"read","arguments":"{}"}}` + "\n"
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Parse(p, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || s.Messages[0].Parts[0].ToolCall == nil || s.Messages[1].Parts[0].ToolCall == nil {
		t.Fatalf("tool calls were not read: %+v", s.Messages)
	}
	if got := s.Messages[0].Parts[0].ToolCall.Status; got != "completed" {
		t.Fatalf("Codex completed status = %q", got)
	}
	unknown := s.Messages[1].Parts[0]
	if unknown.ToolCall.Status != "" || !strings.Contains(string(unknown.Raw), `"status":"queued_for_retry"`) {
		t.Fatalf("unknown Codex status was guessed or discarded: %+v", unknown)
	}
}

func TestDiscoverIsolatesBadFile(t *testing.T) {
	home := t.TempDir()
	a := filepath.Join(home, ".claude", "projects", "project")
	b := filepath.Join(home, ".codex", "sessions", "2026")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(a, "bad.jsonl"), []byte("{bad\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("../testdata/codex.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(b, "good.jsonl"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	ss, errs := Discover(home)
	if len(ss) != 1 || len(errs) != 1 {
		t.Fatalf("got %d sessions, %d errors", len(ss), len(errs))
	}
}

func TestContentBlocksPreserveOrderAndUnknown(t *testing.T) {
	p := filepath.Join(t.TempDir(), "session.jsonl")
	data := `{"type":"user","sessionId":"id","message":{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"data":"example"}},{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"done"}]}]}}` + "\n"
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Parse(p, "claude")
	if err != nil {
		t.Fatal(err)
	}
	parts := s.Messages[0].Parts
	if len(parts) != 3 || parts[0].Text != "look" || parts[1].Type != "image" || len(parts[1].Raw) == 0 || parts[2].ToolResult == nil || parts[2].ToolResult.ID != "c1" || parts[2].ToolResult.Content != "done" {
		t.Fatalf("bad parts: %+v", parts)
	}
}

func TestCodexEventFallbackAndFirstIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		want        int
		answer      string
	}{
		{"event-only", "", 2, "answer"},
		{"duplicate", `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}}` + "\n", 2, "prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "rollout.jsonl")
			data := `{"type":"session_meta","payload":{"id":"child","cwd":"/child"}}` + "\n" + `{"type":"session_meta","payload":{"id":"parent","cwd":"/parent"}}` + "\n" + `{"type":"event_msg","payload":{"type":"user_message","message":"prompt"}}` + "\n" + `{"type":"event_msg","payload":{"type":"agent_message","message":"answer"}}` + "\n" + tc.extra
			if err := os.WriteFile(p, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := Parse(p, "codex")
			if err != nil {
				t.Fatal(err)
			}
			if s.NativeID != "child" || s.Workspace != "/child" || len(s.Messages) != tc.want || s.Messages[tc.want-1].Parts[0].Text != tc.answer {
				t.Fatalf("unexpected session: %+v", s)
			}
		})
	}
}

func TestCodecRoundTripAndCrossProvider(t *testing.T) {
	for _, source := range []struct {
		name  string
		codec Codec
		file  string
	}{{"claude", Claude{}, "claude.jsonl"}, {"codex", Codex{}, "codex.jsonl"}} {
		data, err := os.ReadFile(filepath.Join("..", "testdata", source.file))
		if err != nil {
			t.Fatal(err)
		}
		original, err := source.codec.Read(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []struct {
			name  string
			codec Codec
		}{{"claude", Claude{}}, {"codex", Codex{}}} {
			t.Run(source.name+"_to_"+target.name, func(t *testing.T) {
				var out bytes.Buffer
				if err := target.codec.Write(&out, original); err != nil {
					t.Fatal(err)
				}
				got, err := target.codec.Read(&out)
				if err != nil {
					t.Fatal(err)
				}
				var wantParts, gotParts []string
				flatten := func(s Session) []string {
					var result []string
					for _, m := range s.Messages {
						for _, p := range m.Parts {
							switch p.Type {
							case "text":
								result = append(result, "text:"+p.Text)
							case "tool_call":
								result = append(result, "call:"+p.ToolCall.ID+":"+p.ToolCall.Name)
							case "tool_result":
								result = append(result, "result:"+p.ToolResult.ID+":"+p.ToolResult.Content)
							}
						}
					}
					return result
				}
				wantParts, gotParts = flatten(original), flatten(got)
				if !reflect.DeepEqual(wantParts, gotParts) {
					t.Fatalf("want %v; got %v", wantParts, gotParts)
				}
			})
		}
	}
}
func TestWriterRejectsUnsupportedWithoutOutput(t *testing.T) {
	for _, codec := range []Codec{Claude{}, Codex{}} {
		var out bytes.Buffer
		s := Session{NativeID: "id", Messages: []Message{{Role: "user", Parts: []ContentBlock{{Type: "text", Text: "ok"}, {Type: "image", Raw: json.RawMessage(`{"type":"image"}`)}}}}}
		if err := codec.Write(&out, s); err == nil || out.Len() != 0 {
			t.Fatalf("expected error with no output, got %v %q", err, out.String())
		}
	}
}

// Structured tool results are serialized as exact JSON text instead of
// being dropped; Convert reports the flattening.
func TestWriterSerializesStructuredResult(t *testing.T) {
	structured := json.RawMessage(`[{"type":"text","text":"some text"},{"n":9007199254740993}]`)
	s := Session{NativeID: "123e4567-e89b-12d3-a456-426614174000", Workspace: "/example", Messages: []Message{{Role: "tool", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "tool_result", ToolResult: &ToolResult{ID: "call", Content: "", RawContent: structured}}}}}}
	for _, codec := range []Codec{Claude{}, Codex{}} {
		var out bytes.Buffer
		if err := codec.Write(&out, s); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		if !strings.Contains(out.String(), `\"n\":9007199254740993`) && !strings.Contains(out.String(), `"n":9007199254740993`) {
			t.Fatalf("structured result not serialized exactly:\n%s", out.String())
		}
	}
}

func TestOriginalRestoration(t *testing.T) {
	original := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"id\"}}\r\n{\"type\":\"future_record\",\"payload\":{\"x\":42}}\r\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"hello\"}]}}")
	s, err := (Codex{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	var restored bytes.Buffer
	if err = s.RestoreOriginal(&restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, restored.Bytes()) {
		t.Fatal("original bytes changed")
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var copy Session
	if err = json.Unmarshal(encoded, &copy); err != nil {
		t.Fatal(err)
	}
	same, err := Convert(copy, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, same.Target) || len(same.Limitations) != 0 {
		t.Fatal("same-provider restoration changed the source")
	}
	transfer, err := Convert(copy, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, transfer.Original) || len(transfer.Limitations) == 0 {
		t.Fatal("missing original or limitations")
	}
	readback, err := (Claude{}).Read(bytes.NewReader(transfer.Target))
	if err != nil || readback.Messages[0].Parts[0].Text != "hello" {
		t.Fatalf("unexpected target: %v", err)
	}
}

func TestRestoreRequiresOriginal(t *testing.T) {
	var out bytes.Buffer
	if err := (Session{}).RestoreOriginal(&out); err == nil {
		t.Fatal("missing original accepted")
	}
}
