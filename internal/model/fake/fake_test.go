package fake

import (
	"context"
	"testing"

	"github.com/shreyasprasad/agentd/internal/model"
)

func TestScriptedProvider(t *testing.T) {
	p := New(
		ToolUse("t1", "finish", map[string]string{"answer": "x"}, model.Usage{InputTokens: 10, OutputTokens: 5}),
		Text("done", model.Usage{}),
	).WithPrice(model.Price{InputPerMTok: 1_000_000, OutputPerMTok: 2_000_000})

	ctx := context.Background()
	r1, err := p.Complete(ctx, model.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if r1.StopReason != model.StopToolUse || r1.Model != "m" || len(r1.ToolUses()) != 1 {
		t.Fatalf("r1 = %+v", r1)
	}
	if got := p.CostMicroUSD("m", r1.Usage); got != 10+10 {
		t.Fatalf("cost = %d", got)
	}
	r2, err := p.Complete(ctx, model.Request{Model: "m"})
	if err != nil || r2.Text() != "done" {
		t.Fatalf("r2 = %+v, %v", r2, err)
	}
	if _, err := p.Complete(ctx, model.Request{}); err == nil {
		t.Fatal("expected exhausted script to error")
	}
	if p.Calls() != 3 || len(p.Requests()) != 3 {
		t.Fatalf("calls = %d", p.Calls())
	}
}
