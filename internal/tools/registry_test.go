package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type stubTool struct {
	name   string
	schema string
}

func (s stubTool) Name() string            { return s.name }
func (s stubTool) Description() string     { return "stub " + s.name }
func (s stubTool) Schema() json.RawMessage { return json.RawMessage(s.schema) }
func (s stubTool) TrustTier() TrustTier    { return Builtin }
func (s stubTool) Invoke(context.Context, Invocation) (Result, error) {
	return Result{Content: json.RawMessage(`{}`)}, nil
}

const objSchema = `{"type":"object","properties":{"n":{"type":"integer"},"s":{"type":"string"}},"required":["n"],"additionalProperties":false}`

func TestRegistryRegister(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubTool{"a", objSchema}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubTool{"a", objSchema}); err == nil {
		t.Fatal("duplicate should fail")
	}
	if err := r.Register(stubTool{"", objSchema}); err == nil {
		t.Fatal("empty name should fail")
	}
	if err := r.Register(stubTool{"bad", `{"type": 12}`}); err == nil {
		t.Fatal("invalid schema should fail")
	}
	if err := r.Register(stubTool{"empty", ""}); err != nil {
		t.Fatalf("empty schema should default to object: %v", err)
	}
	if _, ok := r.Get("a"); !ok {
		t.Fatal("Get a")
	}
	if _, ok := r.Get("zzz"); ok {
		t.Fatal("Get zzz should miss")
	}
}

func TestRegistryDefsFollowAllowlist(t *testing.T) {
	r := NewRegistry().MustRegister(stubTool{"a", objSchema}, stubTool{"b", objSchema}, stubTool{"c", objSchema})

	defs := r.Defs([]string{"c", "a", "nope"})
	if len(defs) != 2 || defs[0].Name != "c" || defs[1].Name != "a" {
		t.Fatalf("Defs = %+v", defs)
	}
	if defs[0].Description != "stub c" || string(defs[0].InputSchema) != objSchema {
		t.Fatalf("def c = %+v", defs[0])
	}
	if got := r.Defs(nil); len(got) != 0 {
		t.Fatalf("empty allowlist should yield no tools, got %d", len(got))
	}
}

func TestRegistryResolve(t *testing.T) {
	r := NewRegistry().MustRegister(stubTool{"a", objSchema})
	allow := []string{"a"}

	cases := []struct {
		name    string
		tool    string
		allow   []string
		args    string
		wantErr error
		wantVal bool
	}{
		{name: "ok", tool: "a", allow: allow, args: `{"n":1,"s":"x"}`},
		{name: "unknown", tool: "b", allow: allow, args: `{}`, wantErr: ErrUnknownTool},
		{name: "not allowed", tool: "a", allow: nil, args: `{"n":1}`, wantErr: ErrNotAllowed},
		{name: "missing required", tool: "a", allow: allow, args: `{"s":"x"}`, wantVal: true},
		{name: "wrong type", tool: "a", allow: allow, args: `{"n":"one"}`, wantVal: true},
		{name: "extra property", tool: "a", allow: allow, args: `{"n":1,"zzz":true}`, wantVal: true},
		{name: "not json", tool: "a", allow: allow, args: `{n:`, wantVal: true},
		{name: "empty args treated as object", tool: "a", allow: allow, args: ``, wantVal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, err := r.Resolve(tc.tool, tc.allow, json.RawMessage(tc.args))
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantVal:
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %v, want ValidationError", err)
				}
				if ve.Tool != "a" {
					t.Fatalf("ValidationError.Tool = %q", ve.Tool)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if tool.Name() != "a" {
					t.Fatalf("resolved %q", tool.Name())
				}
			}
		})
	}
}

func TestCostAddAndIsZero(t *testing.T) {
	a := Cost{MicroUSD: 1200, InputTokens: 11000, OutputTokens: 500, Model: "reranker-7b"}
	b := Cost{MicroUSD: 300, InputTokens: 2000, OutputTokens: 80, Model: "other"}

	want := Cost{MicroUSD: 1500, InputTokens: 13000, OutputTokens: 580, Model: "reranker-7b"}
	if got := a.Add(b); got != want {
		t.Fatalf("Add = %+v, want %+v", got, want)
	}
	if got := (Cost{}).Add(b); got.Model != "other" {
		t.Fatalf("an empty cost must take the other's model, got %q", got.Model)
	}
	if !(Cost{}).IsZero() {
		t.Fatal("the zero cost is what every builtin reports")
	}
	if !(Cost{Model: "reranker-7b"}).IsZero() {
		t.Fatal("a model name with no tokens behind it is still nothing spent")
	}
	if a.IsZero() {
		t.Fatal("a priced cost is not zero")
	}
}

func TestTrustTierJSON(t *testing.T) {
	b, _ := json.Marshal(map[string]TrustTier{"t": Sandboxed})
	if string(b) != `{"t":"sandboxed"}` {
		t.Fatalf("got %s", b)
	}
}
