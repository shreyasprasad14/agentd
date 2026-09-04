// Package fake is a scripted ModelProvider for tests. It returns queued
// responses in order and records every request it saw, so a test can assert
// exactly what conversation the loop rebuilt after a resume.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/shreyasprasad/agentd/internal/model"
)

// Provider is a scripted model.
type Provider struct {
	mu        sync.Mutex
	responses []*model.Response
	requests  []model.Request
	price     model.Price
	// OnComplete, if set, is called before each response is returned. Tests
	// use it to block or to fail a call.
	OnComplete func(ctx context.Context, req model.Request, n int) error
}

// New builds a Provider that answers with responses in order. Once the script
// is exhausted every call returns an error, which surfaces as a failed run
// rather than an infinite loop.
func New(responses ...*model.Response) *Provider {
	return &Provider{responses: responses}
}

// WithPrice sets nonzero pricing so budget enforcement can be exercised.
func (p *Provider) WithPrice(price model.Price) *Provider {
	p.price = price
	return p
}

// Name implements model.Provider.
func (p *Provider) Name() string { return "fake" }

// Complete implements model.Provider.
func (p *Provider) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	p.mu.Lock()
	n := len(p.requests)
	p.requests = append(p.requests, req)
	var resp *model.Response
	if n < len(p.responses) {
		resp = p.responses[n]
	}
	hook := p.OnComplete
	p.mu.Unlock()

	if hook != nil {
		if err := hook(ctx, req, n); err != nil {
			return nil, err
		}
	}
	if resp == nil {
		return nil, fmt.Errorf("fake provider: script exhausted after %d calls", n)
	}
	out := *resp
	if out.Model == "" {
		out.Model = req.Model
	}
	return &out, nil
}

// CostMicroUSD implements model.Provider.
func (p *Provider) CostMicroUSD(_ string, u model.Usage) int64 { return p.price.Cost(u) }

// Requests returns a copy of every request seen so far.
func (p *Provider) Requests() []model.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]model.Request(nil), p.requests...)
}

// Calls is the number of completions requested so far.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// Text builds an end_turn response with one text block.
func Text(text string, usage model.Usage) *model.Response {
	return &model.Response{
		Content:    []model.ContentBlock{{Type: model.BlockText, Text: text}},
		StopReason: model.StopEndTurn,
		Usage:      usage,
	}
}

// ToolUse builds a tool_use response for one tool. args must marshal to JSON.
func ToolUse(id, name string, args any, usage model.Usage) *model.Response {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return &model.Response{
		Content: []model.ContentBlock{
			{Type: model.BlockToolUse, ToolUseID: id, Name: name, Input: raw},
		},
		StopReason: model.StopToolUse,
		Usage:      usage,
	}
}
