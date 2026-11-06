// resume-check converts a Codex session to Trae format and verifies the output
// with Ferry's own Trae reader, printing everything the conversion reported as
// lost. It exists to prove the data path before the native CLI resume check
// touches Trae. With no argument (or "-") it uses a small built-in synthetic
// Codex session; with an argument it reads that real rollout file.
package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/williamfzc/ferry/session"
)

func syntheticCodex() []byte {
	nativeID := "123e4567-e89b-12d3-a456-426614174000"
	return []byte(fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-10-27T00:00:00Z","payload":{"id":%q,"cwd":"/tmp/ferry-resume-workspace"}}
{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"ferry native resume check"}]}}
{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ferry resume output ready"}]}}
{"type":"response_item","timestamp":"2026-10-27T00:00:03Z","payload":{"type":"function_call","call_id":"resume-call-1","name":"read","arguments":"{\"path\":\"README.md\"}"}}
{"type":"response_item","timestamp":"2026-10-27T00:00:04Z","payload":{"type":"function_call_output","call_id":"resume-call-1","output":"ferry resume tool result"}}
`, nativeID))
}

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: resume-check [CODEX_ROLLOUT] OUT_JSONL")
		os.Exit(2)
	}
	outPath := os.Args[len(os.Args)-1]
	var data []byte
	if len(os.Args) == 3 && os.Args[1] != "-" {
		var err error
		data, err = os.ReadFile(os.Args[1])
		if err != nil {
			panic(err)
		}
	} else {
		data = syntheticCodex()
	}
	src, err := (session.Codex{}).Read(bytes.NewReader(data))
	if err != nil {
		panic(fmt.Errorf("codex read: %w", err))
	}
	fmt.Printf("source: id=%s native=%s workspace=%q messages=%d\n",
		src.ID, src.NativeID, src.Workspace, len(src.Messages))

	transfer, err := session.Convert(src, "trae")
	if err != nil {
		panic(fmt.Errorf("convert to trae: %w", err))
	}
	fmt.Printf("target: type=%s bytes=%d\n", transfer.TargetType, len(transfer.Target))
	for i, lim := range transfer.Limitations {
		fmt.Printf("limitation[%d]: %s\n", i, lim)
	}
	if err := os.WriteFile(outPath, transfer.Target, 0o600); err != nil {
		panic(err)
	}

	// Ferry read-back: the Trae writer's output must parse back with the same
	// identity and a non-empty message list.
	back, err := (session.Trae{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		panic(fmt.Errorf("trae read-back: %w", err))
	}
	fmt.Printf("read-back: id=%s native=%s workspace=%q messages=%d\n",
		back.ID, back.NativeID, back.Workspace, len(back.Messages))
	if back.NativeID != src.NativeID {
		panic(fmt.Errorf("native id changed: %q -> %q", src.NativeID, back.NativeID))
	}
	if len(back.Messages) == 0 {
		panic("read-back produced no messages")
	}
	fmt.Println("ferry read-back OK")
	fmt.Printf("RESUME_CHECK_NATIVE_ID=%s\n", src.NativeID)
}
