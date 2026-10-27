package session

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func openCodeToolStatusDocument(status string) []byte {
	return []byte(fmt.Sprintf(`{"info":{"id":"ses_status","directory":"/work"},"messages":[{"info":{"id":"msg_status","sessionID":"ses_status","role":"assistant","time":{"created":1}},"parts":[{"type":"tool","callID":"call_status","tool":"read","state":{"status":%q,"input":{"path":"file"},"output":"done","error":"failed","time":{"start":1,"end":2}}}]}]}`, status))
}

func TestOpenCodeReadMapsToolStatusesToACP(t *testing.T) {
	for _, tc := range []struct {
		openCode string
		acp      string
	}{
		{openCode: "pending", acp: "pending"},
		{openCode: "running", acp: "in_progress"},
		{openCode: "completed", acp: "completed"},
		{openCode: "error", acp: "failed"},
	} {
		t.Run(tc.openCode, func(t *testing.T) {
			s, err := (OpenCode{}).Read(bytes.NewReader(openCodeToolStatusDocument(tc.openCode)))
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Messages) == 0 || len(s.Messages[0].Parts) == 0 || s.Messages[0].Parts[0].ToolCall == nil {
				t.Fatalf("tool call was not read: %+v", s.Messages)
			}
			if got := s.Messages[0].Parts[0].ToolCall.Status; got != tc.acp {
				t.Fatalf("OpenCode %q status = %q, want %q", tc.openCode, got, tc.acp)
			}
		})
	}
}

func TestOpenCodeReadDoesNotInferUnknownToolStatus(t *testing.T) {
	s, err := (OpenCode{}).Read(bytes.NewReader(openCodeToolStatusDocument("cancelled")))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || len(s.Messages[0].Parts) != 1 || s.Messages[0].Parts[0].ToolCall == nil {
		t.Fatalf("unexpected messages for unknown status: %+v", s.Messages)
	}
	part := s.Messages[0].Parts[0]
	if got := part.ToolCall.Status; got != "" {
		t.Fatalf("unknown OpenCode status was inferred as %q", got)
	}
	if !strings.Contains(string(part.Raw), `"status":"cancelled"`) {
		t.Fatalf("unknown OpenCode status was not preserved in raw part: %s", part.Raw)
	}
}

func TestConvertReportsUnknownOpenCodeToolStatus(t *testing.T) {
	source, err := (OpenCode{}).Read(bytes.NewReader(openCodeToolStatusDocument("cancelled")))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), `Tool call "call_status" source status "cancelled" has no ACP equivalent`) {
		t.Fatalf("unknown OpenCode status loss was not reported: %v", transfer.Limitations)
	}
}
