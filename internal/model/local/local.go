// Package local is the ModelProvider for on-box runtimes that expose the
// OpenAI-compatible chat completions API: Ollama, llama.cpp's llama-server,
// vLLM. One client covers all of them, which is why the native Ollama API is
// not used.
package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/shreyasprasad/agentd/internal/model"
)

// Config configures the client.
type Config struct {
	// BaseURL is the API root, e.g. http://localhost:11434/v1 for Ollama or
	// http://localhost:8080/v1 for llama-server.
	BaseURL string
	// APIKey is sent as a bearer token when set. Local runtimes ignore it.
	APIKey string
	// Timeout bounds one completion. Cold model loads on a laptop can take
	// a minute; defaults to ten.
	Timeout time.Duration
	// Price is per-model pricing. Empty means everything is free, which is
	// the honest answer for a local model.
	Price map[string]model.Price
	// HTTPClient overrides the client (tests).
	HTTPClient *http.Client
}

// Provider is an OpenAI-compatible chat client.
type Provider struct {
	cfg  Config
	http *http.Client
}

// New builds a Provider.
func New(cfg Config) *Provider {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Provider{cfg: cfg, http: hc}
}

// Name implements model.Provider.
func (p *Provider) Name() string { return "local" }

// CostMicroUSD implements model.Provider.
func (p *Provider) CostMicroUSD(m string, u model.Usage) int64 {
	return p.cfg.Price[m].Cost(u)
}

// MaxOutputTokens implements model.Provider. A request's max_tokens is passed
// through as it arrives, zero included, and the runtime decides for itself, so
// there is no default to report. Nothing is lost by reporting zero: local
// tokens are free, so the budget estimate this feeds never binds here anyway.
func (p *Provider) MaxOutputTokens(string) int { return 0 }

// nonRetryableStatus reports whether an HTTP status from a local runtime
// describes a request that will fail identically however many times it is
// sent: a malformed body, a rejected key, a model that is not installed.
// Everything else — connection failures, 5xx, a truncated body — stays
// retryable, because a runtime that is still loading a model answers with
// exactly those and the retry is the thing that makes it work (ADR-27).
func nonRetryableStatus(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// Wire types: the subset of the chat completions schema we use.

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Stream      bool          `json:"stream"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete implements model.Provider.
func (p *Provider) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	body, err := json.Marshal(toWire(req))
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	debug := os.Getenv("AGENTD_HTTP_DEBUG") != ""
	if debug {
		fmt.Fprintf(os.Stderr, "--- local model request %s ---\n%s\n", httpReq.URL, body)
	}

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("local model: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("local model: read body: %w", err)
	}
	if debug {
		fmt.Fprintf(os.Stderr, "--- local model response %d ---\n%s\n", resp.StatusCode, raw)
	}
	if resp.StatusCode/100 != 2 {
		err := fmt.Errorf("local model: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 512))
		if nonRetryableStatus(resp.StatusCode) {
			return nil, model.NonRetryable(err)
		}
		return nil, err
	}

	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return nil, fmt.Errorf("local model: decode: %w", err)
	}
	if wr.Error != nil {
		return nil, fmt.Errorf("local model: %s", wr.Error.Message)
	}
	if len(wr.Choices) == 0 {
		return nil, fmt.Errorf("local model: no choices in response")
	}
	return fromWire(wr, req.Model), nil
}

// toWire translates content blocks into chat-completions messages. Tool
// results become role:tool messages; everything else maps one to one.
func toWire(req model.Request) wireRequest {
	out := wireRequest{Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature}
	if req.System != "" {
		out.Messages = append(out.Messages, wireMessage{Role: "system", Content: str(req.System)})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case model.RoleAssistant:
			wm := wireMessage{Role: "assistant"}
			var text []string
			for _, b := range m.Content {
				switch b.Type {
				case model.BlockText:
					text = append(text, b.Text)
				case model.BlockToolUse:
					args := string(b.Input)
					if args == "" {
						args = "{}"
					}
					wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
						ID: b.ToolUseID, Type: "function",
						Function: wireFunction{Name: b.Name, Arguments: args},
					})
				}
			}
			if len(text) > 0 {
				wm.Content = str(strings.Join(text, "\n"))
			} else if len(wm.ToolCalls) == 0 {
				wm.Content = str("")
			}
			out.Messages = append(out.Messages, wm)
		default:
			var text []string
			for _, b := range m.Content {
				switch b.Type {
				case model.BlockToolResult:
					out.Messages = append(out.Messages, wireMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: str(b.Content)})
				case model.BlockText:
					text = append(text, b.Text)
				}
			}
			if len(text) > 0 {
				out.Messages = append(out.Messages, wireMessage{Role: "user", Content: str(strings.Join(text, "\n"))})
			}
		}
	}
	for _, t := range req.Tools {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.InputSchema
		if len(wt.Function.Parameters) == 0 {
			wt.Function.Parameters = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, wt)
	}
	return out
}

func fromWire(wr wireResponse, fallbackModel string) *model.Response {
	choice := wr.Choices[0]
	resp := &model.Response{
		Model: wr.Model,
		Usage: model.Usage{InputTokens: wr.Usage.PromptTokens, OutputTokens: wr.Usage.CompletionTokens},
	}
	if resp.Model == "" {
		resp.Model = fallbackModel
	}
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		resp.Content = append(resp.Content, model.ContentBlock{Type: model.BlockText, Text: *choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = "call_" + uuid.NewString()[:8]
		}
		resp.Content = append(resp.Content, model.ContentBlock{
			Type:      model.BlockToolUse,
			ToolUseID: id,
			Name:      tc.Function.Name,
			Input:     argumentsJSON(tc.Function.Arguments),
		})
	}
	switch {
	case len(choice.Message.ToolCalls) > 0:
		resp.StopReason = model.StopToolUse
	case choice.FinishReason == "length":
		resp.StopReason = model.StopMaxTokens
	default:
		resp.StopReason = model.StopEndTurn
	}
	return resp
}

// argumentsJSON keeps tool arguments as valid JSON no matter what the model
// emitted. Garbage becomes a JSON string, which then fails schema validation
// and goes back to the model as an error instead of breaking the event log.
func argumentsJSON(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	quoted, _ := json.Marshal(s)
	return quoted
}

func str(s string) *string { return &s }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
