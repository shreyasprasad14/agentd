package runtime

import (
	"fmt"

	"github.com/shreyasprasad/agentd/internal/model"
)

// charsPerToken converts a character count into a token count. Four is the
// usual rule of thumb for English prose and is close enough for a ceiling:
// the number it produces is only ever added to a figure the provider measured
// itself, so an error here scales with the new content of one step rather
// than with the size of the conversation.
const charsPerToken = 4

// budgetEstimate is the priced worst case of one model call: the prompt as
// large as we believe it to be, and the model writing to its output cap.
type budgetEstimate struct {
	InputTokens     int64
	MaxOutputTokens int64
	MicroUSD        int64
}

// describe renders how the estimate was reached, for the run_finished error
// of a refused call. A refusal that only says "too expensive" is one nobody
// can argue with, and the first question asked of this feature will be why a
// run stopped with budget left.
func (e budgetEstimate) describe() string {
	if e.MaxOutputTokens <= 0 {
		// The local provider: the server decides the cap, and the estimate
		// stands on its input term alone.
		return fmt.Sprintf("%d input tokens, no output cap", e.InputTokens)
	}
	return fmt.Sprintf("%d input + up to %d output tokens", e.InputTokens, e.MaxOutputTokens)
}

// nextCallEstimate prices the worst case of the next model call. It is pure
// over its arguments — no store, no clock — so the arithmetic that decides
// whether a run may keep spending is testable without a database.
//
// The output term is what the request asks for, and the provider's own cap
// when it asks for nothing. A provider that reports no cap (the local one:
// the server decides, and local spend is $0 either way) leaves the estimate
// standing on its input term alone.
func nextCallEstimate(p model.Provider, state *State, req model.Request) budgetEstimate {
	out := int64(req.MaxTokens)
	if out <= 0 {
		out = int64(p.MaxOutputTokens(req.Model))
	}
	if out < 0 {
		// A negative cap would price the call *below* its input term, which
		// is the one direction a ceiling must never round.
		out = 0
	}
	est := budgetEstimate{InputTokens: nextInputTokens(state, req), MaxOutputTokens: out}
	est.MicroUSD = p.CostMicroUSD(req.Model, model.Usage{
		InputTokens:  est.InputTokens,
		OutputTokens: est.MaxOutputTokens,
	})
	return est
}

// nextInputTokens estimates how many prompt tokens the next model call will
// carry, without a tokenizer.
//
// It is sound rather than a guess because of what it is built from: the
// previous step's input_tokens is a number the provider itself measured and
// billed, and the only thing added to the message list since is the tool
// results now sitting in it, whose size is known exactly. So the estimate is
// one measured quantity plus one counted quantity, and the rule of thumb is
// applied only to the counted part.
//
// Before the first response there is nothing measured to build on, so the
// whole request — system prompt, goal, and tool schemas — is counted the same
// way. That branch also catches a provider that reports no usage at all, in
// which case counting the entire conversation is the safe reading.
//
// It deliberately ignores prompt caching. A cached prompt is cheaper than
// this says, and a run may therefore be refused a call it could in fact have
// afforded. For a ceiling that is the right direction to err: the opposite
// failure mode is a bill (ADR-22).
func nextInputTokens(state *State, req model.Request) int64 {
	if state.LastInputTokens == 0 {
		return requestChars(req) / charsPerToken
	}
	return state.LastInputTokens + appendedChars(state.Messages)/charsPerToken
}

// requestChars is the character length of everything a request puts in front
// of the model: the system prompt, every message, and the tool definitions,
// which are part of the prompt the provider bills for even though nothing in
// the conversation mentions them.
func requestChars(req model.Request) int64 {
	n := int64(len(req.System))
	for _, m := range req.Messages {
		n += messageChars(m)
	}
	for _, t := range req.Tools {
		n += int64(len(t.Name) + len(t.Description) + len(t.InputSchema))
	}
	return n
}

// appendedChars is the size of everything added to the message list since the
// most recent assistant turn — that is, the tool results the next call will
// carry and the previous one did not. With no assistant turn yet the whole
// list is new, which is the first-step reading.
func appendedChars(msgs []model.Message) int64 {
	last := -1
	for i := range msgs {
		if msgs[i].Role == model.RoleAssistant {
			last = i
		}
	}
	var n int64
	for _, m := range msgs[last+1:] {
		n += messageChars(m)
	}
	return n
}

// messageChars counts every field of a message that reaches the provider as
// text. Block types the loop treats as opaque (thinking, redacted thinking)
// are counted too: they are sent back verbatim on the next call, so they cost
// prompt tokens like anything else.
func messageChars(m model.Message) int64 {
	var n int64
	for _, b := range m.Content {
		n += int64(len(b.Text) + len(b.Content) + len(b.Input) +
			len(b.Name) + len(b.Thinking) + len(b.Data))
	}
	return n
}
