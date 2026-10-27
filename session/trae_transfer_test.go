package session

import (
	"bytes"
	"strings"
	"testing"
)

func TestConvertTraeReportsUnsupportedCompletedItemTypes(t *testing.T) {
	original := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174022","cwd":"/work/trae"}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"command-1","command":"ls","status":"completed"}}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","id":"reasoning-1","content":[{"type":"reasoning","text":"private"}]}}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"answer-1","content":[{"type":"Text","text":"kept"}]}}}
`)
	source, err := (Trae{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transfer.Original, original) {
		t.Fatal("Trae conversion changed the source snapshot")
	}
	limitations := strings.Join(transfer.Limitations, "\n")
	for _, itemType := range []string{"CommandExecution", "Reasoning"} {
		if !strings.Contains(limitations, "event_msg.item_completed.item="+itemType+"=1") {
			t.Fatalf("unsupported completed item type %q was not reported: %v", itemType, transfer.Limitations)
		}
	}
}
