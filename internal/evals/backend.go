package evals

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/cassette"
	"github.com/shreyasprasad/agentd/internal/model/fake"
)

// CassetteBackend is the replay and record backend: one cassette file per
// case, named by case id.
//
// Replay and record are one type because they are one operation with the miss
// policy flipped. Re-recording after a prompt edit re-asks only the calls that
// actually changed, because every call before the drift still matches its
// recorded entry.
type CassetteBackend struct {
	// Dir holds <case-id>.json.
	Dir string
	// OnMiss is cassette.MissFail for `make eval` and cassette.MissRecord for
	// `make eval-record`. Fail is the default and the only safe value for
	// CI: a cassette that falls through to a live provider turns a free,
	// deterministic suite into a surprise bill.
	OnMiss cassette.OnMiss
	// Live is the provider a recording is taken from. Unused, and never
	// contacted, under MissFail.
	Live model.Provider
	// Model names the backend model for a cassette being created.
	Model string
	// Script, when set, is where a recording's answers come from instead of
	// Live: the case's own scripted trajectory. It is how the checked-in
	// cassettes are regenerated on a machine with no model.
	Script bool
	// Fresh truncates each case's cassette before recording. Script mode sets
	// it, because regenerating from a trajectory that is fully known should
	// leave nothing behind; model mode does not, so re-recording after a
	// prompt edit re-asks only the calls that actually changed.
	Fresh bool

	mu      sync.Mutex
	current *cassette.Provider
	path    string
}

// Path is where a case's cassette lives.
func (b *CassetteBackend) Path(c Case) string {
	return filepath.Join(b.Dir, c.ID+".json")
}

// For implements Backend.
func (b *CassetteBackend) For(c Case) (model.Provider, string, string, error) {
	live := b.Live
	if b.Script {
		scripted, err := ScriptedProvider(c)
		if err != nil {
			return nil, "", "", err
		}
		live = scripted
	}

	path := b.Path(c)
	file, err := b.load(c, path, live)
	if err != nil {
		return nil, "", "", err
	}
	p, err := cassette.New(cassette.Config{File: file, OnMiss: b.OnMiss, Live: live})
	if err != nil {
		return nil, "", "", err
	}
	b.mu.Lock()
	b.current, b.path = p, path
	b.mu.Unlock()
	return p, filepath.Base(path), file.Model, nil
}

func (b *CassetteBackend) load(c Case, path string, live model.Provider) (*cassette.File, error) {
	if b.Fresh && b.OnMiss == cassette.MissRecord {
		return cassette.NewFile(c.ID, live, b.modelName(c), nil), nil
	}
	file, err := cassette.Load(path)
	switch {
	case err == nil:
		return file, nil
	case os.IsNotExist(err) && b.OnMiss == cassette.MissRecord:
		return cassette.NewFile(c.ID, live, b.modelName(c), nil), nil
	case os.IsNotExist(err):
		return nil, fmt.Errorf("%w: %s not found (regenerate with `make eval-record`)", ErrNoCassette, path)
	default:
		return nil, err
	}
}

func (b *CassetteBackend) modelName(c Case) string {
	if c.Model != "" {
		return c.Model
	}
	return b.Model
}

// Finish implements Backend, saving anything the case recorded. A replay that
// recorded nothing does not rewrite the file, so `make eval` leaves the
// working tree clean.
//
// A full regeneration writes the file even with nothing in it. An empty
// cassette is not a missing one: a run the pre-flight budget ceiling stops
// makes zero model calls by design, and what its cassette carries is the rate
// card alone — which is the whole of what that case needs to replay, since the
// estimate it was refused by prices a call that never happened.
func (b *CassetteBackend) Finish(Case) error {
	b.mu.Lock()
	p, path := b.current, b.path
	b.current, b.path = nil, ""
	b.mu.Unlock()
	if p == nil {
		return nil
	}
	if _, _, recorded := p.Stats(); recorded == 0 && !b.Fresh {
		return nil
	}
	return p.File().Save(path)
}

// ScriptedProvider builds the model a case's `script:` describes: a scripted
// backend that answers each call with the next declared turn, priced by
// script_price so a BUDGET case has something to enforce.
//
// It exists to *record* cassettes, not to run the suite. `make eval` replays
// the recorded file through the real matching path; what the script decides is
// which trajectory the case is about, which is a fixture choice and not a
// measurement. The consequence is worth stating plainly rather than leaving
// for a reader to notice: under a scripted recording, every assertion about
// what the model *chose* — an injection it resisted, a citation it wrote — is
// testing the plumbing that carried the choice, not the choice. `make
// eval-live` is where the model is on trial.
func ScriptedProvider(c Case) (model.Provider, error) {
	if len(c.Script) == 0 {
		return nil, fmt.Errorf("case %q has no script to record from; write one, or record from a model with `-from model`", c.ID)
	}
	responses := make([]*model.Response, 0, len(c.Script))
	for i, turn := range c.Script {
		usage := model.Usage{InputTokens: 200, OutputTokens: 40}
		if turn.Usage != nil {
			usage = model.Usage{InputTokens: turn.Usage.InputTokens, OutputTokens: turn.Usage.OutputTokens}
		}
		if turn.Text != "" {
			responses = append(responses, fake.Text(turn.Text, usage))
			continue
		}
		args := turn.Args
		if args == nil {
			args = map[string]any{}
		}
		responses = append(responses, fake.ToolUse(fmt.Sprintf("tu_%d", i+1), turn.Tool, args, usage))
	}
	p := fake.New(responses...).WithName("scripted")
	if c.ScriptPrice != nil {
		p = p.WithPrice(model.Price{
			InputPerMTok:  int64(c.ScriptPrice.InputUSDPerMTok * model.MicroUSD),
			OutputPerMTok: int64(c.ScriptPrice.OutputUSDPerMTok * model.MicroUSD),
		})
	}
	if c.ScriptMaxOutputTokens > 0 {
		p = p.WithMaxOutputTokens(c.ScriptMaxOutputTokens)
	}
	return p, nil
}

// LiveBackend answers every case from a real provider, with no cassette at
// all. It is what `make eval-live` uses: the same cases, scored against a
// model that is actually thinking, which is the half of §12 that measures the
// model rather than the runtime.
type LiveBackend struct {
	Provider model.Provider
	Model    string
}

// For implements Backend.
func (b *LiveBackend) For(c Case) (model.Provider, string, string, error) {
	name := c.Model
	if name == "" {
		name = b.Model
	}
	return b.Provider, "", name, nil
}

// Finish implements Backend.
func (b *LiveBackend) Finish(Case) error { return nil }
