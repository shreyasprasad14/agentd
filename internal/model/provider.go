// Package model defines the ModelProvider seam between the agent loop and any
// chat-completion backend.
//
// The canonical message format is content blocks (text, tool_use,
// tool_result), Anthropic-shaped. Providers translate to and from their own
// wire format; the loop, the reducer, and the event log only ever see these
// types. That is what lets M1.5 add an Anthropic provider without touching the
// loop or the schema.
package model

import (
	"context"
	"encoding/json"
)

// Role is a conversation role. Tool results travel inside a user message as
// tool_result blocks, so there is no separate tool role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Content block types.
const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// Stop reasons a provider may return.
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
)

// ContentBlock is one unit of message content.
type ContentBlock struct {
	Type string `json:"type"`
	// Text is set for text blocks.
	Text string `json:"text,omitempty"`
	// ToolUseID links a tool_use block to its tool_result.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Name is the tool name on a tool_use block.
	Name string `json:"name,omitempty"`
	// Input is the tool arguments on a tool_use block.
	Input json.RawMessage `json:"input,omitempty"`
	// Content is the body of a tool_result block, already enveloped by the loop.
	Content string `json:"content,omitempty"`
	// IsError marks a tool_result that carries an error instead of a result.
	IsError bool `json:"is_error,omitempty"`
}

// Message is one turn of the conversation.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ToolDef is what the model sees for each callable tool.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Usage is token accounting for one completion.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Add sums usage.
func (u Usage) Add(o Usage) Usage {
	return Usage{InputTokens: u.InputTokens + o.InputTokens, OutputTokens: u.OutputTokens + o.OutputTokens}
}

// Request is one completion call.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []ToolDef
	MaxTokens   int
	Temperature *float64
}

// Response is one completion result.
type Response struct {
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// ToolUses returns the tool_use blocks in order.
func (r *Response) ToolUses() []ContentBlock {
	var out []ContentBlock
	for _, b := range r.Content {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// Text concatenates the text blocks.
func (r *Response) Text() string {
	var s string
	for _, b := range r.Content {
		if b.Type == BlockText {
			if s != "" {
				s += "\n"
			}
			s += b.Text
		}
	}
	return s
}

// Provider is the seam the loop calls into. Implementations must be safe for
// concurrent use; a worker may run several runs at once in later milestones.
type Provider interface {
	// Name identifies the backend ("local", "anthropic", "fake").
	Name() string
	// Complete performs one chat completion with tool calling.
	Complete(ctx context.Context, req Request) (*Response, error)
	// CostMicroUSD prices a completion in millionths of a dollar. Local
	// providers return 0; the value is still recorded so budget enforcement
	// exercises the same path regardless of backend.
	CostMicroUSD(model string, u Usage) int64
}
