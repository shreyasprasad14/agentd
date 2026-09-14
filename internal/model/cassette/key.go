package cassette

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/shreyasprasad/agentd/internal/model"
)

// DefaultVolatile is the field list every cassette gets unless it names its
// own. Each entry is here because it changes between two runs that asked the
// model the same thing:
//
//   - duration_ms   run_python's wall clock.
//   - chunk_id, document_id, document.id
//     store.IngestDocument mints a fresh uuid.New() for every chunk on every
//     ingest, so these are not stable even across two ingests of the same
//     file into the same database — let alone a fresh one in CI.
//   - score
//     stable in principle (RRF over ranks), but float formatting is not worth
//     betting a suite on.
//
// The list is a hole by construction: a tool that starts returning a new
// volatile field breaks replay until this learns about it. The alternative —
// teaching the cassette which fields each tool produces — couples the eval
// harness to every tool's payload shape, and a declared list a miss message
// can point at is the smaller mistake (ADR-28).
func DefaultVolatile() []string {
	return []string{"duration_ms", "chunk_id", "document_id", "score", "document.id"}
}

// Key is what a cassette entry is looked up by: the request fingerprint,
// taken after the fields named by vol have been stripped out of every tool
// result.
//
// It is a hash of the request rather than the call's ordinal because model
// calls are at-least-once (ADR-4): a worker that dies between model_requested
// and model_responded leaves a dangling request that the next worker simply
// repeats, and under ordinal matching that repeat would consume the next entry
// and desynchronise everything after it — failing every RESILIENCE case for a
// reason that has nothing to do with the runtime (ADR-28).
func Key(req model.Request, vol []string) string {
	return model.HashRequest(Normalize(req, vol))
}

// Normalize returns req with every tool_result block rewritten: the envelope's
// seq attribute dropped, and the volatile fields stripped from its JSON body.
// The original is not modified.
//
// Dropping seq is not cosmetic. A run that crashes inside a model call comes
// back with a second model_requested in the log, so every event after it sits
// one seq higher than it would have without the crash — and the seq is printed
// into the envelope of every subsequent tool result. Hashing it would make a
// resumed run's later calls miss a cassette recorded from a clean one. The seq
// is a fact about the log's shape, not about what the model was asked.
func Normalize(req model.Request, vol []string) model.Request {
	set := newVolatileSet(vol)
	out := req
	out.Messages = make([]model.Message, len(req.Messages))
	for i, m := range req.Messages {
		out.Messages[i] = m
		var blocks []model.ContentBlock
		for j, b := range m.Content {
			if b.Type != model.BlockToolResult {
				continue
			}
			normalized := normalizeEnvelope(b.Content, set)
			if normalized == b.Content {
				continue
			}
			if blocks == nil {
				blocks = make([]model.ContentBlock, len(m.Content))
				copy(blocks, m.Content)
			}
			blocks[j].Content = normalized
		}
		if blocks != nil {
			out.Messages[i].Content = blocks
		}
	}
	return out
}

// envelopeRE matches what runtime.Envelope produces. The two are coupled and
// there is no import that would enforce it — a model provider must not drag in
// the loop, the store, and the telemetry stack — so the coupling is asserted by
// a test in internal/evals, which imports both.
var envelopeRE = regexp.MustCompile(`(?s)\A<tool_result tool="([^"]*)" seq=\d+>\n(.*)\n</tool_result>\z`)

// normalizeEnvelope rewrites one enveloped tool result. A body that is not an
// envelope, or not JSON, is passed through: tool_failed results are plain
// "error: ..." text, and a tool is free to return something that is neither.
func normalizeEnvelope(s string, set volatileSet) string {
	m := envelopeRE.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	return `<tool_result tool="` + m[1] + `">` + "\n" + stripJSON(m[2], set) + "\n</tool_result>"
}

// stripJSON removes the volatile fields from a JSON document and re-renders it.
// Go marshals map keys in sorted order, so the output is canonical regardless
// of how the tool serialised it.
func stripJSON(body string, set volatileSet) string {
	if set.empty() {
		return body
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return body
	}
	raw, err := json.Marshal(set.strip(v, ""))
	if err != nil {
		return body
	}
	return string(raw)
}

// volatileSet is the parsed form of a volatile list. A plain name matches that
// key at any depth; a dotted name matches one path from the body's root.
type volatileSet struct {
	keys  map[string]struct{}
	paths map[string]struct{}
}

func newVolatileSet(vol []string) volatileSet {
	s := volatileSet{keys: map[string]struct{}{}, paths: map[string]struct{}{}}
	for _, v := range vol {
		v = strings.TrimSpace(v)
		switch {
		case v == "":
		case strings.Contains(v, "."):
			s.paths[v] = struct{}{}
		default:
			s.keys[v] = struct{}{}
		}
	}
	return s
}

func (s volatileSet) empty() bool { return len(s.keys) == 0 && len(s.paths) == 0 }

// strip deletes matching keys in place and returns v. Array elements inherit
// their container's path, so "hits.chunk_id" matches that field inside every
// element of hits — there is no index syntax, because a path that names one
// element of a result list is never what anybody means.
func (s volatileSet) strip(v any, path string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if _, ok := s.keys[k]; ok {
				delete(t, k)
				continue
			}
			if _, ok := s.paths[p]; ok {
				delete(t, k)
				continue
			}
			t[k] = s.strip(child, p)
		}
	case []any:
		for i, child := range t {
			t[i] = s.strip(child, path)
		}
	}
	return v
}
