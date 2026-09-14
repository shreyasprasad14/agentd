package model

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Router is a Provider that picks a backend per request from the model name,
// so one worker can serve runs against a local model and runs against a
// hosted API from the same queue. Resolution, in order:
//
//  1. An explicit "<provider>/<model>" prefix, e.g. "anthropic/claude-opus-5"
//     or "local/qwen2.5:7b". The prefix is stripped before the backend sees
//     the model name.
//  2. A backend's Match function, e.g. the Anthropic provider claims every
//     bare "claude-*" model.
//  3. The default backend.
//
// The same rule prices a response, so CostMicroUSD and Complete always agree
// on which backend a model belongs to.
type Router struct {
	mu       sync.RWMutex
	backends map[string]route
	// order keeps Match checks deterministic when two backends could claim
	// a name: registration order wins.
	order       []string
	defaultName string
}

type route struct {
	provider Provider
	match    func(model string) bool
}

// NewRouter builds an empty Router. Register at least one backend and set a
// default before use.
func NewRouter() *Router {
	return &Router{backends: map[string]route{}}
}

// Register adds a backend under name. match may be nil; when set it claims
// bare model names (no "<provider>/" prefix) the function accepts.
func (r *Router) Register(name string, p Provider, match func(model string) bool) *Router {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.backends[name]; dup {
		panic(fmt.Sprintf("model.Router: backend %q registered twice", name))
	}
	r.backends[name] = route{provider: p, match: match}
	r.order = append(r.order, name)
	if r.defaultName == "" {
		r.defaultName = name
	}
	return r
}

// SetDefault names the backend for model names nothing else claims. The
// first registered backend is the default until this is called.
func (r *Router) SetDefault(name string) *Router {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.backends[name]; !ok {
		panic(fmt.Sprintf("model.Router: no backend %q to make default", name))
	}
	r.defaultName = name
	return r
}

// Backends lists registered backend names, sorted.
func (r *Router) Backends() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}

// Name implements Provider. Responses carry the resolved backend's name in
// Response.Provider, which is what the event log records.
func (r *Router) Name() string { return "router" }

// Backend names the provider that will serve model, and the model name that
// provider will be handed. It is what a caller labelling a metric wants: a
// composite provider's own Name is "router", which would put every hosted and
// local call in one series, and Response.Provider only exists once a call has
// succeeded — so a failed attempt would be filed under a different provider
// than a successful one.
//
// An unroutable model reports the composite's name and the name as given.
// The Complete call for it is about to fail with the routing error, and the
// metric should say which model nobody could serve.
func Backend(p Provider, name string) (provider, model string) {
	if r, ok := p.(*Router); ok {
		if backend, _, backendModel, err := r.Resolve(name); err == nil {
			return backend, backendModel
		}
	}
	return p.Name(), name
}

// Resolve returns the backend for model and the model name to hand it.
func (r *Router) Resolve(model string) (name string, p Provider, backendModel string, err error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.backends) == 0 {
		return "", nil, "", fmt.Errorf("model router: no backends registered")
	}
	if i := strings.IndexByte(model, '/'); i > 0 {
		prefix, rest := model[:i], model[i+1:]
		rt, ok := r.backends[prefix]
		if !ok {
			return "", nil, "", fmt.Errorf("model router: no provider %q for model %q (have %s)", prefix, model, strings.Join(r.order, ", "))
		}
		if rest == "" {
			return "", nil, "", fmt.Errorf("model router: empty model name after %q/", prefix)
		}
		return prefix, rt.provider, rest, nil
	}
	for _, name := range r.order {
		rt := r.backends[name]
		if rt.match != nil && rt.match(model) {
			return name, rt.provider, model, nil
		}
	}
	rt := r.backends[r.defaultName]
	return r.defaultName, rt.provider, model, nil
}

// Complete implements Provider.
func (r *Router) Complete(ctx context.Context, req Request) (*Response, error) {
	name, p, backendModel, err := r.Resolve(req.Model)
	if err != nil {
		return nil, err
	}
	req.Model = backendModel
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Provider == "" {
		resp.Provider = name
	}
	return resp, nil
}

// CostMicroUSD implements Provider. An unroutable model prices at zero; the
// Complete call for it has already failed, so nothing was spent.
func (r *Router) CostMicroUSD(model string, u Usage) int64 {
	_, p, backendModel, err := r.Resolve(model)
	if err != nil {
		return 0
	}
	return p.CostMicroUSD(backendModel, u)
}

// MaxOutputTokens implements Provider by asking the backend that would serve
// the model, so the loop's pre-flight estimate uses the cap the call will
// actually run under rather than the composite's guess. An unroutable model
// reports zero for the same reason it prices at zero: the Complete call for it
// is about to fail, so there is no call to bound.
func (r *Router) MaxOutputTokens(model string) int {
	_, p, backendModel, err := r.Resolve(model)
	if err != nil {
		return 0
	}
	return p.MaxOutputTokens(backendModel)
}
