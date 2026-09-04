// Package builtin holds the in-process tools every run can be granted.
package builtin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/shreyasprasad/agentd/internal/tools"
)

// FinishName is the terminal tool's name.
const FinishName = "finish"

// Finish is the explicit terminal tool: completion is a decision the model
// makes by calling it, not something the loop infers from a parse.
type Finish struct{}

// FinishArgs is the finish tool's input.
type FinishArgs struct {
	Answer string `json:"answer"`
}

func (Finish) Name() string { return FinishName }

func (Finish) Description() string {
	return "Call this when the goal is complete. Pass the final answer for the user in `answer`. This ends the run."
}

func (Finish) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "answer": {"type": "string", "description": "The final answer to deliver to the user."}
  },
  "required": ["answer"],
  "additionalProperties": false
}`)
}

func (Finish) TrustTier() tools.TrustTier { return tools.Builtin }

func (Finish) Invoke(_ context.Context, inv tools.Invocation) (tools.Result, error) {
	var args FinishArgs
	if err := json.Unmarshal(inv.Args, &args); err != nil {
		return tools.Result{}, err
	}
	if args.Answer == "" {
		return tools.Result{}, errors.New("answer must not be empty")
	}
	out, _ := json.Marshal(map[string]string{"answer": args.Answer})
	return tools.Result{Content: out, Terminal: true}, nil
}

// FinalAnswer extracts the answer from a finish result. It is what the loop
// records in run_finished.
func FinalAnswer(result json.RawMessage) string {
	var args FinishArgs
	_ = json.Unmarshal(result, &args)
	return args.Answer
}
