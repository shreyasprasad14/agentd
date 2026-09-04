package model

import (
	"encoding/json"
	"testing"
)

func TestResponseHelpers(t *testing.T) {
	r := Response{Content: []ContentBlock{
		{Type: BlockText, Text: "first"},
		{Type: BlockToolUse, ToolUseID: "a", Name: "x", Input: json.RawMessage(`{}`)},
		{Type: BlockText, Text: "second"},
		{Type: BlockToolUse, ToolUseID: "b", Name: "y", Input: json.RawMessage(`{}`)},
	}}
	if got := r.Text(); got != "first\nsecond" {
		t.Fatalf("Text = %q", got)
	}
	uses := r.ToolUses()
	if len(uses) != 2 || uses[0].ToolUseID != "a" || uses[1].ToolUseID != "b" {
		t.Fatalf("ToolUses = %+v", uses)
	}
	if got := (&Response{}).Text(); got != "" {
		t.Fatalf("empty Text = %q", got)
	}
}

func TestContentBlockJSONOmitsEmpty(t *testing.T) {
	b, err := json.Marshal(ContentBlock{Type: BlockText, Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"type":"text","text":"hi"}` {
		t.Fatalf("got %s", b)
	}
}
