package tools

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"sync"

	"encoding/json"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/shreyasprasad/agentd/internal/model"
)

// ErrUnknownTool is returned for names not in the registry.
var ErrUnknownTool = errors.New("unknown tool")

// ErrNotAllowed is returned when a run's allowlist excludes the tool.
var ErrNotAllowed = errors.New("tool not allowed for this run")

// ValidationError wraps a JSON Schema failure. The loop reports it to the
// model as a non-retryable tool failure.
type ValidationError struct {
	Tool string
	Err  error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid arguments for %s: %v", e.Tool, e.Err)
}
func (e *ValidationError) Unwrap() error { return e.Err }

type entry struct {
	tool   Tool
	schema *jsonschema.Schema
}

// Registry maps tool names to implementations and their compiled schemas.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]entry
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]entry{}}
}

// Register adds a tool, compiling its schema. Duplicate names and invalid
// schemas are errors at registration so a bad tool fails at boot, not
// mid-run.
func (r *Registry) Register(t Tool) error {
	name := t.Name()
	if name == "" {
		return errors.New("register: tool has empty name")
	}
	sch, err := compileSchema(name, t.Schema())
	if err != nil {
		return fmt.Errorf("register %s: %w", name, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("register: duplicate tool %q", name)
	}
	r.tools[name] = entry{tool: t, schema: sch}
	return nil
}

// MustRegister is Register that panics; for wiring builtins at boot.
func (r *Registry) MustRegister(ts ...Tool) *Registry {
	for _, t := range ts {
		if err := r.Register(t); err != nil {
			panic(err)
		}
	}
	return r
}

// Get looks a tool up by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.tools[name]
	return e.tool, ok
}

// List returns every registered tool sorted by name.
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, e := range r.tools {
		out = append(out, e.tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Names returns every registered tool name sorted.
func (r *Registry) Names() []string {
	ts := r.List()
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}

// Defs renders the model-facing definitions for the tools in allowlist, in
// allowlist order, skipping names that are not registered. An empty allowlist
// yields no tools: the allowlist is fixed at submission time and the API
// fills in the default, so the loop never has to guess.
func (r *Registry) Defs(allowlist []string) []model.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]model.ToolDef, 0, len(allowlist))
	for _, name := range allowlist {
		e, ok := r.tools[name]
		if !ok {
			continue
		}
		out = append(out, model.ToolDef{
			Name:        name,
			Description: e.tool.Description(),
			InputSchema: e.tool.Schema(),
		})
	}
	return out
}

// Resolve returns the tool for an invocation after checking the run's
// allowlist and validating args against the schema. Every error it returns
// is a reason to tell the model "no", never a reason to crash the run.
func (r *Registry) Resolve(name string, allowlist []string, args json.RawMessage) (Tool, error) {
	r.mu.RLock()
	e, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}
	allowed := false
	for _, n := range allowlist {
		if n == name {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("%w: %s", ErrNotAllowed, name)
	}
	if err := validate(e.schema, args); err != nil {
		return nil, &ValidationError{Tool: name, Err: err}
	}
	return e.tool, nil
}

func compileSchema(name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{"type":"object"}`)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	url := "agentd://tools/" + name + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

func validate(sch *jsonschema.Schema, args json.RawMessage) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	return sch.Validate(v)
}
