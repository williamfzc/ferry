package session

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func openCodeToolStateDocument(state string) []byte {
	return []byte(fmt.Sprintf(`{"info":{"id":"ses_state","directory":"/work"},"messages":[{"info":{"id":"msg_state","sessionID":"ses_state","role":"assistant","time":{"created":1}},"parts":[{"type":"tool","callID":"call_state","tool":"read"%s}]}]}`, state))
}

func TestOpenCodeReadRejectsMissingOrMalformedToolState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		want  string
	}{
		{name: "missing", state: ``, want: "missing state"},
		{name: "null", state: `,"state":null`, want: "missing state"},
		{name: "scalar", state: `,"state":"running"`, want: "expected an object"},
		{name: "missing status", state: `,"state":{"input":{}}`, want: "status is required"},
		{name: "invalid status", state: `,"state":{"status":true,"input":{}}`, want: "status must be a non-empty string"},
		{name: "missing input", state: `,"state":{"status":"running"}`, want: "input is required"},
		{name: "null input", state: `,"state":{"status":"running","input":null}`, want: "input is required"},
		{name: "completed without output", state: `,"state":{"status":"completed","input":{}}`, want: "output is required"},
		{name: "error without error", state: `,"state":{"status":"error","input":{}}`, want: "error is required"},
		{name: "error has wrong type", state: `,"state":{"status":"error","input":{},"error":true}`, want: "cannot unmarshal bool into Go struct field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (OpenCode{}).Read(bytes.NewReader(openCodeToolStateDocument(tc.state)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Read error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestOpenCodeReadAcceptsStructuredUnknownToolState(t *testing.T) {
	data := openCodeToolStateDocument(`,"state":{"status":"cancelled","input":"legacy JSON arguments"}`)
	s, err := (OpenCode{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || len(s.Messages[0].Parts) != 1 || s.Messages[0].Parts[0].ToolCall == nil {
		t.Fatalf("unexpected messages for unknown state: %+v", s.Messages)
	}
	if got := s.Messages[0].Parts[0].ToolCall.Status; got != "" {
		t.Fatalf("unknown OpenCode status was inferred as %q", got)
	}
}
