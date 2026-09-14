// Package cassette records a run's model calls to a file and replays them, so
// the eval suite exercises the whole runtime with no model, no API key, and no
// network (spec §12).
//
// It replaces the model and nothing else. A replayed run still writes to
// Postgres, still searches the real corpus, and still executes real containers
// — which is the point: what is being measured is the loop, the ledger, the
// envelope, the budget gates, and the resume path, and freezing those too
// would leave nothing under test.
package cassette

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shreyasprasad/agentd/internal/model"
)

// Version is the cassette file format version. A file from a different version
// is refused rather than guessed at.
const Version = 1

// Price is the recorded per-token price, in micro-USD per million tokens. It
// is in the header because the pre-flight budget ceiling (ADR-22) prices a
// call that has not happened yet, using usage the cassette has never seen: a
// replayed BUDGET case therefore needs a real price function, not a recorded
// number. With it, a run recorded against Claude replays its exact termination
// arithmetic on a machine with no API key.
type Price struct {
	InputPerMTok      int64 `json:"input_per_mtok_micro_usd"`
	OutputPerMTok     int64 `json:"output_per_mtok_micro_usd"`
	CacheReadPerMTok  int64 `json:"cache_read_per_mtok_micro_usd,omitempty"`
	CacheWritePerMTok int64 `json:"cache_write_per_mtok_micro_usd,omitempty"`
}

func (p Price) price() model.Price {
	return model.Price{
		InputPerMTok:      p.InputPerMTok,
		OutputPerMTok:     p.OutputPerMTok,
		CacheReadPerMTok:  p.CacheReadPerMTok,
		CacheWritePerMTok: p.CacheWritePerMTok,
	}
}

// probePrice recovers a provider's rate card through the Provider interface
// rather than by reaching into the implementation. Price.Cost is linear with a
// divide by a million, so pricing exactly one million tokens of one kind
// returns that kind's rate exactly.
func probePrice(p model.Provider, name string) Price {
	const mtok = 1_000_000
	return Price{
		InputPerMTok:      p.CostMicroUSD(name, model.Usage{InputTokens: mtok}),
		OutputPerMTok:     p.CostMicroUSD(name, model.Usage{OutputTokens: mtok}),
		CacheReadPerMTok:  p.CostMicroUSD(name, model.Usage{CacheReadInputTokens: mtok}),
		CacheWritePerMTok: p.CostMicroUSD(name, model.Usage{CacheCreationInputTokens: mtok}),
	}
}

// Entry is one recorded model call.
type Entry struct {
	// Ordinal is the order the call was recorded in. It is for the miss
	// message, never for matching.
	Ordinal int `json:"ordinal"`
	// Key is the normalised fingerprint hash, and is what lookup uses.
	Key string `json:"key"`
	// Exact is model.HashRequest of the unmodified request — the same value
	// the model_requested event carries. Recorded for diagnosis only, so
	// "the cassette matched but the request was not byte-identical" is a
	// question with an answer.
	Exact string `json:"exact"`
	// Request is the normalised fingerprint itself, so a miss is a diff
	// rather than two hashes that merely differ.
	Request  model.Fingerprint `json:"request"`
	Response *model.Response   `json:"response"`
}

// File is a cassette on disk: one per eval case.
type File struct {
	Version    int       `json:"version"`
	Case       string    `json:"case,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
	// Provider and Model name the backend the recording came from. Provider
	// is what the replaying Provider reports as its own name, so the event
	// log of a replayed run says "anthropic" where the recording did.
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Price    Price  `json:"price"`
	// MaxOutputTokens is the recorded provider's answer for this model. The
	// budget estimate's output term reads it.
	MaxOutputTokens int `json:"max_output_tokens"`
	// Volatile is the field list stripped before hashing. It lives in the
	// file rather than in code so a case can override it and so a miss can
	// point at it.
	Volatile []string `json:"volatile"`
	Entries  []Entry  `json:"entries"`
}

// NewFile starts an empty cassette for recording against live.
func NewFile(caseID string, live model.Provider, modelName string, vol []string) *File {
	if len(vol) == 0 {
		vol = DefaultVolatile()
	}
	return &File{
		Version:         Version,
		Case:            caseID,
		RecordedAt:      time.Now().UTC(),
		Provider:        live.Name(),
		Model:           modelName,
		Price:           probePrice(live, modelName),
		MaxOutputTokens: live.MaxOutputTokens(modelName),
		Volatile:        vol,
	}
}

// Load reads a cassette.
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != Version {
		return nil, fmt.Errorf("%s: cassette version %d, this build reads %d (re-record with `make eval-record`)",
			path, f.Version, Version)
	}
	if len(f.Volatile) == 0 {
		f.Volatile = DefaultVolatile()
	}
	return &f, nil
}

// Save writes a cassette, creating its directory.
func (f *File) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// OnMiss says what a Provider does with a request the cassette has no entry
// for.
type OnMiss string

const (
	// MissFail is the default and the only safe value for `make eval`: a
	// missing entry fails the call with a diff. A cassette that transparently
	// falls through to a live provider turns "deterministic and free" into a
	// surprise bill and a test that passed for the wrong reason (ADR-28).
	MissFail OnMiss = "fail"
	// MissRecord calls the live provider and appends the answer. This is
	// recording mode, and it is why recording and replaying are one type:
	// re-recording a cassette after a prompt edit only re-asks the calls that
	// actually changed.
	MissRecord OnMiss = "record"
)

// Config wires a Provider.
type Config struct {
	File   *File
	OnMiss OnMiss
	// Live is the backend a miss is recorded from. Required for MissRecord,
	// and never contacted under MissFail.
	Live model.Provider
}

// Provider is a model.Provider backed by a cassette.
type Provider struct {
	mu     sync.Mutex
	file   *File
	index  map[string]int
	cfg    Config
	hits   int
	misses int
	// recorded counts entries this process appended, so a caller knows
	// whether the file on disk is now stale.
	recorded int
}

// New wires a Provider over a cassette.
func New(cfg Config) (*Provider, error) {
	if cfg.File == nil {
		return nil, fmt.Errorf("cassette: File is required")
	}
	if cfg.OnMiss == "" {
		cfg.OnMiss = MissFail
	}
	if cfg.OnMiss == MissRecord && cfg.Live == nil {
		return nil, fmt.Errorf("cassette: -on-miss=record needs a live provider")
	}
	p := &Provider{file: cfg.File, cfg: cfg, index: map[string]int{}}
	for i, e := range cfg.File.Entries {
		if _, dup := p.index[e.Key]; dup {
			// Two entries for one request would make replay depend on which
			// one won, which is exactly the nondeterminism a cassette exists
			// to remove.
			return nil, fmt.Errorf("cassette %q: duplicate entry for key %s (ordinals collide)", cfg.File.Case, e.Key[:12])
		}
		p.index[e.Key] = i
	}
	return p, nil
}

// File returns the cassette, including anything recorded since it was loaded.
func (p *Provider) File() *File { return p.file }

// Stats reports replay coverage.
func (p *Provider) Stats() (hits, misses, recorded int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits, p.misses, p.recorded
}

// Name implements model.Provider. It reports the recorded backend rather than
// "cassette", so a replayed run's event log and spans name the provider the
// trajectory actually came from.
func (p *Provider) Name() string {
	if p.file.Provider == "" {
		return "cassette"
	}
	return p.file.Provider
}

// CostMicroUSD implements model.Provider from the recorded rate card. The
// model argument is ignored: a run targets one model, and pricing a call the
// cassette did not record under a model it did not record would invent a
// number.
func (p *Provider) CostMicroUSD(_ string, u model.Usage) int64 {
	return p.file.Price.price().Cost(u)
}

// MaxOutputTokens implements model.Provider from the recorded header.
func (p *Provider) MaxOutputTokens(string) int { return p.file.MaxOutputTokens }

// Complete implements model.Provider. A hit returns the recorded response; a
// repeated identical request — which is what a resume produces — hits the same
// entry rather than consuming the next one.
func (p *Provider) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	norm := Normalize(req, p.file.Volatile)
	fp := model.NewFingerprint(norm)
	key := fp.Hash()

	p.mu.Lock()
	i, ok := p.index[key]
	if ok {
		p.hits++
		resp := *p.file.Entries[i].Response
		p.mu.Unlock()
		return &resp, nil
	}
	p.misses++
	record := p.cfg.OnMiss == MissRecord
	p.mu.Unlock()

	if !record {
		return nil, p.missError(fp)
	}
	resp, err := p.cfg.Live.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.index[key]; !dup {
		p.file.Entries = append(p.file.Entries, Entry{
			Ordinal:  len(p.file.Entries) + 1,
			Key:      key,
			Exact:    model.HashRequest(req),
			Request:  fp,
			Response: resp,
		})
		p.index[key] = len(p.file.Entries) - 1
		p.recorded++
	}
	out := *resp
	return &out, nil
}

// MissError is what a replay miss fails with. It carries the drift rather
// than only reporting one, so the fix — re-record, or add a field to the
// volatile list — is visible from the failure.
type MissError struct {
	Case string
	// Nearest is the recorded entry the drift is described against: the one
	// whose conversation is closest to the request that missed.
	Nearest *Entry
	Got     model.Fingerprint
	Drift   string
	Entries int
}

func (e *MissError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cassette %q has no entry for this request (%d recorded)", e.Case, e.Entries)
	if e.Drift != "" {
		fmt.Fprintf(&b, "\n  %s", e.Drift)
	}
	b.WriteString("\n  re-record with `make eval-record`, or add the field that moved to the cassette's \"volatile\" list")
	return b.String()
}

func (p *Provider) missError(got model.Fingerprint) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	nearest := p.nearest(got)
	e := &MissError{Case: p.file.Case, Nearest: nearest, Got: got, Entries: len(p.file.Entries)}
	if nearest != nil {
		e.Drift = describeDrift(nearest.Request, got)
	}
	return e
}

// nearest picks the recorded entry to describe the drift against: the one
// sharing the longest prefix of messages with the request that missed, which
// for a conversation that is appended to is the call just before the drift.
func (p *Provider) nearest(got model.Fingerprint) *Entry {
	best, bestShared := -1, -1
	for i := range p.file.Entries {
		shared := commonPrefix(p.file.Entries[i].Request.Messages, got.Messages)
		// >= so that among equally-close entries the later one wins, which is
		// the one the run had got to.
		if shared >= bestShared {
			best, bestShared = i, shared
		}
	}
	if best < 0 {
		return nil
	}
	return &p.file.Entries[best]
}

func commonPrefix(a, b []model.Message) int {
	n := 0
	for n < len(a) && n < len(b) {
		x, _ := json.Marshal(a[n])
		y, _ := json.Marshal(b[n])
		if string(x) != string(y) {
			break
		}
		n++
	}
	return n
}

// describeDrift names the first thing that differs between a recorded request
// and a live one, in the order a reader would check them.
func describeDrift(want, got model.Fingerprint) string {
	switch {
	case want.Model != got.Model:
		return fmt.Sprintf("model changed: recorded %q, asked %q", want.Model, got.Model)
	case want.System != got.System:
		return fmt.Sprintf("system prompt changed (recorded %d chars, asked %d): %s",
			len(want.System), len(got.System), firstDiff(want.System, got.System))
	case strings.Join(want.Tools, ",") != strings.Join(got.Tools, ","):
		return fmt.Sprintf("tools changed: recorded [%s], asked [%s]",
			strings.Join(want.Tools, " "), strings.Join(got.Tools, " "))
	}
	n := commonPrefix(want.Messages, got.Messages)
	switch {
	case n == len(want.Messages) && n == len(got.Messages):
		return "the fingerprint matches field by field but not as a whole; this is a bug in the cassette key"
	case n >= len(want.Messages):
		return fmt.Sprintf("conversation is longer than the recording: %d messages, recorded %d — "+
			"the loop took a step the recording did not", len(got.Messages), len(want.Messages))
	case n >= len(got.Messages):
		return fmt.Sprintf("conversation is shorter than the recording: %d messages, recorded %d",
			len(got.Messages), len(want.Messages))
	}
	w, _ := json.Marshal(want.Messages[n])
	g, _ := json.Marshal(got.Messages[n])
	return fmt.Sprintf("message %d of %d differs (role %s): %s",
		n, len(got.Messages), got.Messages[n].Role, firstDiff(string(w), string(g)))
}

// firstDiff renders the neighbourhood of the first differing byte, which for
// a tool result whose duration moved is the whole story in one line.
func firstDiff(want, got string) string {
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	const window = 90
	lo := max(0, i-20)
	return fmt.Sprintf("at byte %d\n    recorded: …%s…\n    asked:    …%s…",
		i, excerpt(want, lo, window), excerpt(got, lo, window))
}

func excerpt(s string, from, n int) string {
	if from > len(s) {
		return ""
	}
	s = s[from:]
	if len(s) > n {
		s = s[:n]
	}
	return strings.ReplaceAll(s, "\n", "\\n")
}
