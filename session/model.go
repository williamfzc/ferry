// Package session defines a common representation for agent histories.
package session

import (
	"encoding/json"
	"time"
)

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Namespace qualifies MCP tool calls (Codex "mcp__server"). It is kept so
	// writers can emit a fully qualified name instead of a bare tool name.
	Namespace string          `json:"namespace,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	// Status uses ACP v1's tool-call vocabulary when the source carries a
	// status with the same meaning. Provider-specific statuses that have no
	// faithful ACP equivalent remain in ContentBlock.Raw. Provider writers do
	// not emit this field by itself: they only write statuses when their
	// provider-specific raw block already carries a verified representation.
	Status string `json:"status,omitempty"`
}
type ToolResult struct {
	ID      string `json:"id"`
	Content string `json:"content,omitempty"`
	// IsError marks a failed tool execution (Claude "is_error", OpenCode
	// state.status "error"). Providers without a flag cannot represent it.
	IsError    bool            `json:"is_error,omitempty"`
	RawContent json.RawMessage `json:"raw_content,omitempty"`
}

// ContentBlock preserves the order and original representation of message parts.
// Unknown types remain available through Raw rather than being discarded.
type ContentBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCall   *ToolCall       `json:"tool_call,omitempty"`
	ToolResult *ToolResult     `json:"tool_result,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}
type Message struct {
	Role      string         `json:"role"`
	Parts     []ContentBlock `json:"parts,omitempty"`
	Timestamp string         `json:"timestamp,omitempty"`
}
type Session struct {
	ID         string    `json:"id"`
	SourceType string    `json:"source_type"`
	SourcePath string    `json:"source_path"`
	NativeID   string    `json:"native_id"`
	Workspace  string    `json:"workspace,omitempty"`
	Updated    time.Time `json:"updated"`
	Messages   []Message `json:"messages"`
	Original   []byte    `json:"original,omitempty"`
}

// acpToolCallStatus maps only statuses whose meaning is established across
// the supported providers and ACP v1. An empty result means that the source
// status must stay in the original provider block rather than being guessed.
func acpToolCallStatus(status string) string {
	switch status {
	case "pending", "validating", "scheduled", "awaiting_approval":
		return "pending"
	case "in_progress", "executing":
		return "in_progress"
	case "completed", "success":
		return "completed"
	case "failed", "error":
		return "failed"
	default:
		return ""
	}
}

func isACPToolCallStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "completed", "failed":
		return true
	default:
		return false
	}
}
