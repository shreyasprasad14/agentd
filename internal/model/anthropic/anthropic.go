// Package anthropic is the ModelProvider for Claude via the Anthropic
// Messages API, through the official Go SDK.
//
// The runtime's canonical message format is already Anthropic-shaped
// (ADR-2), so the translation here is mostly one-to-one. What this package
// adds over the local provider is what a hosted API needs and a local one
// does not: real per-token pricing so the budget path is exercised with
// actual dollars, thinking blocks carried through the event log so a
// reasoning turn that calls a tool can be continued, and refusals surfaced
// as errors rather than as empty answers.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/shreyasprasad/agentd/internal/model"
)

// DefaultModel is the model used when a run names none and the worker's
// default routes here.
const DefaultModel = "claude-opus-5"

// DefaultMaxTokens caps one completion when the run's agent_config sets no
// max_tokens. Thinking output counts against it, so it is generous.
const DefaultMaxTokens = 16000

// Config configures the client.
type Config struct {
	// APIKey authenticates requests. Empty defers to the SDK's own lookup:
	// ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or a profile from `ant auth
	// login`.
	APIKey string
	// BaseURL overrides the API root (tests, proxies). Empty uses the SDK
	// default or ANTHROPIC_BASE_URL.
	BaseURL string
	// Timeout bounds one completion including SDK retries. Defaults to ten
	// minutes; long thinking turns on the largest models can take a while.
	Timeout time.Duration
	// MaxRetries is how many times the SDK retries 408/409/429/5xx and
	// connection errors before returning. Zero means the SDK default (2);
	// negative disables retries. The loop retries on top of this.
	MaxRetries int
	// Price adds to or overrides DefaultPrices, keyed by model id (prefix
	// match). A model with no price cannot be called.
	Price map[string]model.Price
	// ThinkingDisplay, when set, sends thinking: {type: adaptive, display:
	// X} so the returned thinking blocks carry readable text ("summarized")
	// or only a signature ("omitted"). Empty sends no thinking parameter and
	// takes the model's default, which on current models is adaptive with
	// the text omitted.
	ThinkingDisplay string
	// HTTPClient overrides the transport (tests).
	HTTPClient *http.Client
}

// Provider is a Messages API client.
type Provider struct {
	cfg    Config
	client sdk.Client
	prices priceTable
}

// New builds a Provider.
func New(cfg Config) *Provider {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	opts := []option.RequestOption{option.WithRequestTimeout(cfg.Timeout)}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	switch {
	case cfg.MaxRetries < 0:
		opts = append(opts, option.WithMaxRetries(0))
	case cfg.MaxRetries > 0:
		opts = append(opts, option.WithMaxRetries(cfg.MaxRetries))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	if os.Getenv("AGENTD_HTTP_DEBUG") != "" {
		opts = append(opts, option.WithMiddleware(debugMiddleware))
	}
	return &Provider{cfg: cfg, client: sdk.NewClient(opts...), prices: newPriceTable(cfg.Price)}
}

// Name implements model.Provider.
func (p *Provider) Name() string { return "anthropic" }

// IsClaudeModel reports whether a bare model id belongs to this provider. The
// Router uses it so runs can say "claude-opus-5" without a provider prefix.
func IsClaudeModel(m string) bool { return strings.HasPrefix(m, "claude-") }

// CostMicroUSD implements model.Provider. Unknown models price at zero here
// because Complete has already refused them.
func (p *Provider) CostMicroUSD(m string, u model.Usage) int64 {
	price, _ := p.prices.lookup(m)
	return price.Cost(u)
}

// Price returns the rate card for a model, if known.
func (p *Provider) Price(m string) (model.Price, bool) { return p.prices.lookup(m) }

// RefusalError is returned when the API declines to answer (stop_reason
// "refusal"). It is not retryable; the loop's retries are bounded so a
// refused run fails within a few seconds rather than looping.
type RefusalError struct {
	Model       string
	Category    string
	Explanation string
}

func (e *RefusalError) Error() string {
	msg := fmt.Sprintf("anthropic: %s refused the request", e.Model)
	if e.Category != "" {
		msg += " (" + e.Category + ")"
	}
	if e.Explanation != "" {
		msg += ": " + e.Explanation
	}
	return msg
}

// Complete implements model.Provider.
func (p *Provider) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	if req.Model == "" {
		return nil, errors.New("anthropic: model is required")
	}
	if _, ok := p.prices.lookup(req.Model); !ok {
		return nil, fmt.Errorf("anthropic: no price configured for model %q; budget enforcement needs one (set AGENTD_ANTHROPIC_PRICES or Config.Price)", req.Model)
	}
	params, err := toParams(req, p.cfg)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}

	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		var apiErr *sdk.Error
		if errors.As(err, &apiErr) {
			return nil, fmt.Errorf("anthropic: HTTP %d %s: %w", apiErr.StatusCode, apiErr.Type(), err)
		}
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	if msg.StopReason == sdk.StopReasonRefusal {
		return nil, &RefusalError{
			Model:       string(msg.Model),
			Category:    string(msg.StopDetails.Category),
			Explanation: msg.StopDetails.Explanation,
		}
	}
	return fromMessage(msg, req.Model), nil
}

// toParams translates a canonical request into SDK params.
func toParams(req model.Request, cfg Config) (sdk.MessageNewParams, error) {
	params := sdk.MessageNewParams{
		Model:     sdk.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
	}
	if params.MaxTokens <= 0 {
		params.MaxTokens = DefaultMaxTokens
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System}}
	}
	if req.Temperature != nil {
		params.Temperature = sdk.Float(*req.Temperature)
	}
	if cfg.ThinkingDisplay != "" {
		params.Thinking = sdk.ThinkingConfigParamUnion{OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{
			Display: sdk.ThinkingConfigAdaptiveDisplay(cfg.ThinkingDisplay),
		}}
	}

	for i, m := range req.Messages {
		blocks, err := toBlocks(m.Content)
		if err != nil {
			return params, fmt.Errorf("message %d: %w", i, err)
		}
		switch m.Role {
		case model.RoleAssistant:
			params.Messages = append(params.Messages, sdk.NewAssistantMessage(blocks...))
		case model.RoleUser:
			params.Messages = append(params.Messages, sdk.NewUserMessage(blocks...))
		default:
			return params, fmt.Errorf("message %d: unknown role %q", i, m.Role)
		}
	}

	for _, t := range req.Tools {
		tool := sdk.ToolParam{Name: t.Name}
		if t.Description != "" {
			tool.Description = sdk.String(t.Description)
		}
		schema, err := toInputSchema(t.InputSchema)
		if err != nil {
			return params, fmt.Errorf("tool %s: input schema: %w", t.Name, err)
		}
		tool.InputSchema = schema
		params.Tools = append(params.Tools, sdk.ToolUnionParam{OfTool: &tool})
	}
	return params, nil
}

// toInputSchema converts the registry's raw JSON Schema into the SDK's typed
// param without losing keywords. The SDK types only "properties" and
// "required"; everything else ("additionalProperties", "$schema", ...) has
// to travel in ExtraFields or it is silently dropped from the wire.
func toInputSchema(raw json.RawMessage) (sdk.ToolInputSchemaParam, error) {
	var out sdk.ToolInputSchemaParam
	if len(bytes.TrimSpace(raw)) == 0 {
		return out, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return out, err
	}
	if typ, ok := m["type"]; ok && typ != "object" {
		return out, fmt.Errorf("tool input schema must have type object, got %v", typ)
	}
	for k, v := range m {
		switch k {
		case "type":
		case "properties":
			out.Properties = v
		case "required":
			list, ok := v.([]any)
			if !ok {
				return out, fmt.Errorf("required must be an array")
			}
			for _, item := range list {
				s, ok := item.(string)
				if !ok {
					return out, fmt.Errorf("required must be an array of strings")
				}
				out.Required = append(out.Required, s)
			}
		default:
			if out.ExtraFields == nil {
				out.ExtraFields = map[string]any{}
			}
			out.ExtraFields[k] = v
		}
	}
	return out, nil
}

func toBlocks(content []model.ContentBlock) ([]sdk.ContentBlockParamUnion, error) {
	out := make([]sdk.ContentBlockParamUnion, 0, len(content))
	for _, b := range content {
		switch b.Type {
		case model.BlockText:
			out = append(out, sdk.NewTextBlock(b.Text))
		case model.BlockToolUse:
			var input any = map[string]any{}
			if len(bytes.TrimSpace(b.Input)) > 0 {
				if err := json.Unmarshal(b.Input, &input); err != nil {
					return nil, fmt.Errorf("tool_use %s: input: %w", b.ToolUseID, err)
				}
			}
			out = append(out, sdk.NewToolUseBlock(b.ToolUseID, input, b.Name))
		case model.BlockToolResult:
			out = append(out, sdk.NewToolResultBlock(b.ToolUseID, b.Content, b.IsError))
		case model.BlockThinking:
			out = append(out, sdk.NewThinkingBlock(b.Signature, b.Thinking))
		case model.BlockRedactedThinking:
			out = append(out, sdk.NewRedactedThinkingBlock(b.Data))
		default:
			return nil, fmt.Errorf("unsupported content block type %q", b.Type)
		}
	}
	return out, nil
}

// fromMessage translates an SDK response. Thinking blocks are kept so the
// next request can echo them; the loop and reducer never look at them.
func fromMessage(msg *sdk.Message, fallbackModel string) *model.Response {
	resp := &model.Response{
		Model: string(msg.Model),
		Usage: model.Usage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
		},
	}
	if resp.Model == "" {
		resp.Model = fallbackModel
	}
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			resp.Content = append(resp.Content, model.ContentBlock{Type: model.BlockText, Text: b.Text})
		case "tool_use":
			input := json.RawMessage(b.Input)
			if len(bytes.TrimSpace(input)) == 0 {
				input = json.RawMessage(`{}`)
			}
			resp.Content = append(resp.Content, model.ContentBlock{
				Type: model.BlockToolUse, ToolUseID: b.ID, Name: b.Name, Input: input,
			})
		case "thinking":
			resp.Content = append(resp.Content, model.ContentBlock{
				Type: model.BlockThinking, Thinking: b.Thinking, Signature: b.Signature,
			})
		case "redacted_thinking":
			resp.Content = append(resp.Content, model.ContentBlock{Type: model.BlockRedactedThinking, Data: b.Data})
		default:
			// Server-tool blocks (web search, code execution) cannot occur:
			// no server tools are ever declared. Anything new is dropped
			// rather than guessed at.
		}
	}
	switch msg.StopReason {
	case sdk.StopReasonToolUse:
		resp.StopReason = model.StopToolUse
	case sdk.StopReasonMaxTokens, sdk.StopReasonModelContextWindowExceeded:
		resp.StopReason = model.StopMaxTokens
	case sdk.StopReasonEndTurn, sdk.StopReasonStopSequence, sdk.StopReasonPauseTurn, "":
		resp.StopReason = model.StopEndTurn
	default:
		resp.StopReason = string(msg.StopReason)
	}
	return resp
}

// debugMiddleware dumps request and response bodies to stderr when
// AGENTD_HTTP_DEBUG is set, mirroring the local provider.
func debugMiddleware(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		fmt.Fprintf(os.Stderr, "--- anthropic request %s ---\n%s\n", req.URL, body)
	}
	resp, err := next(req)
	if err != nil || resp == nil {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	fmt.Fprintf(os.Stderr, "--- anthropic response %d ---\n%s\n", resp.StatusCode, body)
	return resp, nil
}
