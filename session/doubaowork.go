// This file reads DoubaoWork (豆包工作) agent trajectories. Each chat is a
// session directory under DoubaoWork/Default/.doubaowork/agent_mode/workspace/
// .sessions/<chat-id>/, and each agent in it (m_ main, s_ subagent,
// o_ organizer) keeps an OpenAI-style message log at
// agents/<agent-id>/system/trajectory.jsonl. Ferry reads it but cannot write it.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type doubaoWorkRecord struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"` // a JSON string or, in newer builds, an object
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID    string   `json:"tool_call_id"`
	ImageLinkList []string `json:"image_link_list"`
}

func parseDoubaoWorkFile(r io.Reader, path string) (Session, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Session{}, err
	}
	s, err := readDoubaoWork(bytes.NewReader(data))
	if err != nil {
		return s, err
	}
	s.Original = data
	agentDir := filepath.Dir(filepath.Dir(path))
	s.NativeID = filepath.Base(filepath.Dir(filepath.Dir(agentDir))) // chat id, as `doubao --resume` takes
	// The trajectory often omits the opening request; assignment.md keeps every
	// request with its timestamp.
	// ponytail: only a missing first request is restored; later gaps have no
	// position in the timestamp-free trajectory, so they stay missing.
	if len(s.Messages) > 0 && s.Messages[0].Role != "user" {
		if a, err := os.ReadFile(filepath.Join(agentDir, "system", "assignment.md")); err == nil {
			if m := assignmentRequest.FindSubmatch(a); m != nil {
				first := Message{Role: "user", Timestamp: string(m[1]), Parts: []ContentBlock{{Type: "text", Text: strings.TrimSpace(string(m[2]))}}}
				s.Messages = append([]Message{first}, s.Messages...)
			}
		}
	}
	return s, nil
}

// assignmentRequest matches the first "## [<timestamp>] 需求" section of assignment.md.
var assignmentRequest = regexp.MustCompile(`(?s)\n## \[([^\]]+)\] 需求\n(.*?)(?:\n## \[|\z)`)

func readDoubaoWork(r io.Reader) (Session, error) {
	s := Session{SourceType: "doubaowork"}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 64*1024*1024)
	for line := 1; scan.Scan(); line++ {
		if len(bytes.TrimSpace(scan.Bytes())) == 0 {
			continue
		}
		var rec doubaoWorkRecord
		if err := json.Unmarshal(scan.Bytes(), &rec); err != nil {
			return s, fmt.Errorf(":%d: invalid doubaowork record: %w", line, err)
		}
		var parts []ContentBlock
		if rec.Role == "tool" {
			parts = append(parts, ContentBlock{Type: "tool_result", ToolResult: &ToolResult{ID: rec.ToolCallID, Content: rec.Content}})
		} else if rec.Content != "" {
			parts = append(parts, ContentBlock{Type: "text", Text: rec.Content})
		}
		for _, c := range rec.ToolCalls {
			input := c.Function.Arguments
			var str string
			if json.Unmarshal(input, &str) == nil && json.Valid([]byte(str)) {
				input = json.RawMessage(str) // stringified JSON object
			}
			parts = append(parts, ContentBlock{Type: "tool_call", ToolCall: &ToolCall{ID: c.ID, Name: c.Function.Name, Input: input}})
		}
		if len(parts) > 0 {
			s.Messages = append(s.Messages, Message{Role: rec.Role, Parts: parts})
		}
	}
	return s, scan.Err()
}

// skippedDoubaoWorkLimitations reports trajectory data outside the common
// projection: image links attached to tool results.
func skippedDoubaoWorkLimitations(s Session) []string {
	images := 0
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		var rec doubaoWorkRecord
		if json.Unmarshal(line, &rec) == nil {
			images += len(rec.ImageLinkList)
		}
	}
	if images == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d DoubaoWork tool-result image link(s) are not represented; the original bytes retain them.", images)}
}
