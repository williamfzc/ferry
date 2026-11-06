package session

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var doubaoWorkFixture = []byte(`{"role":"assistant","content":"looking","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}
{"role":"tool","content":"a.txt","tool_call_id":"c1","image_link_list":["https://x/1.png"]}
{"role":"assistant","content":null,"tool_calls":[{"id":"c2","type":"function","function":{"name":"bad","arguments":"not json"}}]}
{"role":"assistant","content":"","tool_calls":[{"id":"c3","type":"function","function":{"name":"obj","arguments":{"k":1}}}]}
{"role":"user","content":"second ask"}
{"role":"assistant","content":"done"}
`)

const doubaoWorkAssignment = "# Assignment\n\n- Agent ID: m_1\n- Role: main\n\n## [2026-01-01T00:00:00.5Z] 需求\nfirst ask\nline two\n\n## [2026-01-02T00:00:00Z] 需求\nsecond ask\n"

func TestDoubaoWorkParseAndDiscover(t *testing.T) {
	home := t.TempDir()
	base := "Library/Application Support/DoubaoWork/Default/.doubaowork/agent_mode/workspace/.sessions/chat1/agents/"
	writeTree(t, home, map[string][]byte{
		base + "m_1/system/trajectory.jsonl": doubaoWorkFixture,
		base + "m_1/system/assignment.md":    []byte(doubaoWorkAssignment),
		base + "m_1/system/other.jsonl":      doubaoWorkFixture,
	})
	if got, want := discoveredRel(t, home), []string{"doubaowork:" + base + "m_1/system/trajectory.jsonl"}; !slices.Equal(got, want) {
		t.Fatalf("discovered %v, want %v", got, want)
	}

	s, err := Parse(filepath.Join(home, base, "m_1/system/trajectory.jsonl"), "doubaowork")
	if err != nil {
		t.Fatal(err)
	}
	if s.NativeID != "chat1" || len(s.Messages) != 7 {
		t.Fatalf("id %q, %d messages: %+v", s.NativeID, len(s.Messages), s.Messages)
	}
	first := s.Messages[0]
	if first.Role != "user" || first.Parts[0].Text != "first ask\nline two" || first.Timestamp != "2026-01-01T00:00:00.5Z" {
		t.Fatalf("first request not restored from assignment.md: %+v", first)
	}
	if c := s.Messages[1].Parts[1].ToolCall; c == nil || c.Name != "bash" || string(c.Input) != `{"cmd":"ls"}` {
		t.Fatalf("tool call: %+v", s.Messages[1].Parts)
	}
	if r := s.Messages[2].Parts[0].ToolResult; r == nil || r.ID != "c1" || r.Content != "a.txt" {
		t.Fatalf("tool result: %+v", s.Messages[2].Parts)
	}
	if c := s.Messages[3].Parts[0].ToolCall; c == nil || string(c.Input) != `"not json"` {
		t.Fatalf("invalid arguments not wrapped: %+v", s.Messages[3].Parts)
	}
	if c := s.Messages[4].Parts[0].ToolCall; c == nil || string(c.Input) != `{"k":1}` {
		t.Fatalf("object arguments: %+v", s.Messages[4].Parts)
	}
	if lim := skippedDoubaoWorkLimitations(s); len(lim) != 1 || !strings.Contains(lim[0], "1 DoubaoWork") {
		t.Fatalf("limitations: %v", lim)
	}
}
