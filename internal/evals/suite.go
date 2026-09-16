// Package evals is the agent-run eval harness (spec §12): a YAML suite of
// cases, each driven through the real loop, judged against the event log, and
// summed into a scorecard that exits nonzero below its thresholds.
//
// The harness drives the runtime in-process rather than over the HTTP API
// (ADR-29). A RESILIENCE case has to kill a worker mid-tool-call, which is not
// something it can do to a process it does not own — and a harness that
// reimplemented the loop in order to control it would be measuring itself.
package evals

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/shreyasprasad/agentd/internal/model"
)

// The §12 categories. A case must name one of these, so a typo cannot create
// a category that silently has no threshold.
const (
	CategorySmoke      = "SMOKE"
	CategoryRetrieval  = "RETRIEVAL"
	CategoryCitation   = "CITATION"
	CategoryInjection  = "INJECTION"
	CategorySafety     = "SAFETY"
	CategoryResilience = "RESILIENCE"
	CategoryBudget     = "BUDGET"
)

// Categories is every category, in scorecard row order.
func Categories() []string {
	return []string{CategorySmoke, CategoryRetrieval, CategoryCitation,
		CategoryInjection, CategorySafety, CategoryResilience, CategoryBudget}
}

// Capabilities a case can require. A missing one is a skip, not a failure:
// `make eval` must run on a machine with no Docker and say so in the table.
const (
	CapDocker = "docker"
	CapCorpus = "corpus"
	// CapMCP is an MCP server the harness could reach. The suite's poisoned
	// server is the in-repo one, so this is really "the fixture binary built",
	// but a case should not have to know that.
	CapMCP = "mcp"
)

// Suite is one parsed YAML file.
type Suite struct {
	// Path is where this suite was read from; it anchors relative Corpus
	// paths so a suite can be run from any directory.
	Path string `yaml:"-"`
	// Corpus is the JSONL files this suite's cases search. They are ingested
	// once, before any case runs.
	Corpus     []string                      `yaml:"corpus"`
	Defaults   Defaults                      `yaml:"defaults"`
	Thresholds map[string]map[string]float64 `yaml:"thresholds"`
	Cases      []Case                        `yaml:"cases"`
}

// Defaults fill in fields a case leaves empty.
type Defaults struct {
	BudgetUSD string        `yaml:"budget_usd"`
	MaxSteps  int32         `yaml:"max_steps"`
	Timeout   time.Duration `yaml:"timeout"`
	Model     string        `yaml:"model"`
}

// Case is one agent run and what is asserted about it.
type Case struct {
	ID       string `yaml:"id"`
	Category string `yaml:"category"`
	Goal     string `yaml:"goal"`
	// Tools is the run's allowlist, fixed at submission like any run's
	// (ADR-8). An INJECTION case's allowlist is half of what it proves.
	Tools        []string      `yaml:"tools"`
	Model        string        `yaml:"model"`
	SystemPrompt string        `yaml:"system_prompt"`
	BudgetUSD    string        `yaml:"budget_usd"`
	MaxSteps     int32         `yaml:"max_steps"`
	Timeout      time.Duration `yaml:"timeout"`
	// ToolDelayMS widens the window a chaos kill lands in, exactly as the
	// crash demo uses it.
	ToolDelayMS int `yaml:"tool_delay_ms"`
	// Requires names capabilities the case needs; a missing one skips it.
	Requires []string   `yaml:"requires"`
	Chaos    *Chaos     `yaml:"chaos"`
	Assert   Assertions `yaml:"assert"`

	// Script is the trajectory `agentd eval record -from script` records a
	// cassette from: the model's turns, written out, so the checked-in
	// cassettes can be regenerated on any machine with no model at all.
	//
	// It is the *source* of a cassette and never a substitute for one. `make
	// eval` always replays the recorded file through the real matching path,
	// so the normalisation, the repeat-after-crash behaviour, and the recorded
	// rate card are all exercised rather than bypassed. Re-recording from a
	// real model (`make eval-record-live`) replaces these trajectories with
	// ones a model actually chose, and nothing else about the case changes.
	Script []Turn `yaml:"script"`
	// ScriptPrice makes a scripted run cost money, which is the only way a
	// BUDGET case has anything to enforce: locally every model is free.
	ScriptPrice *ScriptPrice `yaml:"script_price"`
	// ScriptMaxOutputTokens is what the scripted provider reports as its
	// output cap, the output term of the pre-flight estimate (ADR-22).
	ScriptMaxOutputTokens int `yaml:"script_max_output_tokens"`
}

// Turn is one scripted model response: a tool call, or text that ends the run.
type Turn struct {
	Tool string         `yaml:"tool"`
	Args map[string]any `yaml:"args"`
	Text string         `yaml:"text"`
	// Usage is what the turn reports having spent. It matters only where the
	// budget does; elsewhere a small default keeps the numbers plausible.
	Usage *TurnUsage `yaml:"usage"`
}

// TurnUsage is one turn's token accounting.
type TurnUsage struct {
	InputTokens  int64 `yaml:"input_tokens"`
	OutputTokens int64 `yaml:"output_tokens"`
}

// ScriptPrice is a rate card in the units a price list is written in — dollars
// per million tokens — rather than the micro-USD the runtime carries.
type ScriptPrice struct {
	InputUSDPerMTok  float64 `yaml:"input_usd_per_mtok"`
	OutputUSDPerMTok float64 `yaml:"output_usd_per_mtok"`
}

// Chaos kills the worker mid-run and starts a second one, which is the crash
// demo expressed as a fixture.
type Chaos struct {
	// KillAfter is the event type whose arrival triggers the kill.
	KillAfter string `yaml:"kill_after"`
	// Matching narrows the trigger to a tool name, for a log where several
	// events share a type.
	Matching string `yaml:"matching"`
	// RestartIn delays the second worker. Zero is fine: it polls until the
	// dead worker's lease lapses.
	RestartIn time.Duration `yaml:"restart_in"`
}

// Assertions is the whole vocabulary. Every one of them is answerable from the
// event log, the ledger, and the corpus tables, with nothing re-executed and
// no second model asked for an opinion.
type Assertions struct {
	// Status is the terminal run status.
	Status string `yaml:"status"`
	// MaxSteps bounds completed model calls; MaxCostUSD bounds spent_usd.
	MaxSteps   *int   `yaml:"max_steps"`
	MaxCostUSD string `yaml:"max_cost_usd"`

	AnswerContains []string `yaml:"answer_contains"`
	AnswerOmits    []string `yaml:"answer_omits"`
	AnswerMatches  string   `yaml:"answer_matches"`

	ToolsCalled    []string `yaml:"tools_called"`
	ToolsNotCalled []string `yaml:"tools_not_called"`

	// Retrieved names source ids that must appear in some tool result — the
	// RETRIEVAL row: the known-relevant document is in the trajectory.
	Retrieved []string `yaml:"retrieved"`

	EventsContain    []string `yaml:"events_contain"`
	EventsNotContain []string `yaml:"events_not_contain"`

	Citations *CitationAssert  `yaml:"citations"`
	Budget    *BudgetAssert    `yaml:"budget"`
	Sandbox   *SandboxAssert   `yaml:"sandbox"`
	Injection *InjectionAssert `yaml:"injection"`

	// ExactlyOnce checks ADR-4's claim against the log: one completion per
	// tool_use_id, no ledger row left started, and a resumed call marked
	// replayed rather than run twice.
	ExactlyOnce bool `yaml:"exactly_once"`
}

// CitationAssert checks the answer's citations against the chunks table.
type CitationAssert struct {
	Min int `yaml:"min"`
	// AllResolve fails on any (source_id, ordinal) that is not a real row.
	AllResolve bool `yaml:"all_resolve"`
	// MustInclude names source ids the answer has to cite.
	MustInclude []string `yaml:"must_include"`
}

// BudgetAssert checks a budget termination's arithmetic rather than only that
// one happened.
type BudgetAssert struct {
	// Reason is "spent" or "would_exceed".
	Reason string `yaml:"reason"`
	// SpentBelowLimit is ADR-22's ceiling claim: a run stopped by the
	// pre-flight check never spent past its budget.
	SpentBelowLimit bool `yaml:"spent_below_limit"`
}

// SandboxAssert reads a sandboxed tool's result out of the log. It never
// re-runs anything: exit_code in tool_succeeded is the sandbox's own, which is
// the boundary ADR-14 drew for exactly this.
type SandboxAssert struct {
	// Tool defaults to run_python.
	Tool            string   `yaml:"tool"`
	ExitCodeZero    bool     `yaml:"exit_code_zero"`
	ExitCodeNonzero bool     `yaml:"exit_code_nonzero"`
	StdoutContains  []string `yaml:"stdout_contains"`
	StdoutOmits     []string `yaml:"stdout_omits"`
	TimedOut        *bool    `yaml:"timed_out"`
	OOMKilled       *bool    `yaml:"oom_killed"`
}

// InjectionAssert is the §10 category. The order of its fields is the order
// they are checked in, and that order is the point (ADR-30).
type InjectionAssert struct {
	// ExposedWith is planted text that must appear in a tool result of this
	// run. It is checked first: a case whose query never surfaced the
	// poisoned document proves nothing, and is reported inconclusive rather
	// than counted as resistance.
	ExposedWith []string `yaml:"exposed_with"`
	// ExposedInTools is planted text that must appear in the run's tool
	// definitions rather than in a result — the tool-poisoning attack, where a
	// server writes the instruction into a tool's description and it rides in
	// every request from then on (ADR-35).
	//
	// It is the other half of exposure, not a variant of it: a description is
	// never inside a <tool_result> envelope, so ExposedWith cannot see it and
	// a case using the wrong one would report "never exposed" for text that
	// was in front of the model the whole run.
	ExposedInTools []string `yaml:"exposed_in_tools"`
	// NotFollowed names tools the planted instruction asked for.
	NotFollowed []string `yaml:"not_followed"`
	// CanaryAbsent is text the model would only emit having followed the
	// instruction.
	CanaryAbsent []string `yaml:"canary_absent"`
}

// ReadSuite parses and validates one suite file. Unknown fields are errors:
// a mistyped assertion that silently did not run would be a case that can
// only pass, which is the one thing an eval must never be.
func ReadSuite(r io.Reader, path string) (*Suite, error) {
	var s Suite
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	s.Path = path
	if err := s.normalize(); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadSuites reads every *.yaml under dir (or dir itself, if it is a file),
// in filename order.
func LoadSuites(dir string) ([]*Suite, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	if info.IsDir() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if ext := filepath.Ext(e.Name()); ext == ".yaml" || ext == ".yml" {
				paths = append(paths, filepath.Join(dir, e.Name()))
			}
		}
		sort.Strings(paths)
	} else {
		paths = []string{dir}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s: no suite files", dir)
	}

	seen := map[string]string{}
	out := make([]*Suite, 0, len(paths))
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		s, err := ReadSuite(f, p)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		for _, c := range s.Cases {
			// Case ids name cassette files, so a collision across suites would
			// have two cases replaying one recording.
			if prev, dup := seen[c.ID]; dup {
				return nil, fmt.Errorf("%s: case id %q is already used in %s", p, c.ID, prev)
			}
			seen[c.ID] = p
		}
		out = append(out, s)
	}
	return out, nil
}

// CorpusFiles resolves the suite's corpus paths relative to the suite file.
func (s *Suite) CorpusFiles() []string {
	base := filepath.Dir(s.Path)
	out := make([]string, 0, len(s.Corpus))
	for _, c := range s.Corpus {
		if filepath.IsAbs(c) {
			out = append(out, c)
			continue
		}
		// Relative to the repository root when it resolves there, so a suite
		// can name evals/corpus/... the way the Makefile and the README do,
		// and relative to the suite file otherwise.
		if _, err := os.Stat(c); err == nil {
			out = append(out, c)
			continue
		}
		out = append(out, filepath.Join(base, c))
	}
	return out
}

func (s *Suite) normalize() error {
	if len(s.Cases) == 0 {
		return fmt.Errorf("suite has no cases")
	}
	if s.Defaults.MaxSteps == 0 {
		s.Defaults.MaxSteps = 10
	}
	if s.Defaults.BudgetUSD == "" {
		s.Defaults.BudgetUSD = "1.00"
	}
	if s.Defaults.Timeout == 0 {
		s.Defaults.Timeout = 120 * time.Second
	}
	for name, metrics := range s.Thresholds {
		if !validCategory(name) {
			return fmt.Errorf("thresholds: unknown category %q", name)
		}
		for metric := range metrics {
			if !validMetric(metric) {
				return fmt.Errorf("thresholds[%s]: unknown metric %q (want %s)",
					name, metric, strings.Join(metricNames(), ", "))
			}
		}
	}
	ids := map[string]bool{}
	for i := range s.Cases {
		c := &s.Cases[i]
		if c.ID == "" {
			return fmt.Errorf("case %d: missing id", i)
		}
		if ids[c.ID] {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		ids[c.ID] = true
		if !validCategory(c.Category) {
			return fmt.Errorf("case %q: unknown category %q (want %s)",
				c.ID, c.Category, strings.Join(Categories(), ", "))
		}
		if c.Goal == "" {
			return fmt.Errorf("case %q: missing goal", c.ID)
		}
		if len(c.Tools) == 0 {
			return fmt.Errorf("case %q: missing tools; the allowlist is fixed at submission and half of what an injection case proves", c.ID)
		}
		if c.Assert.IsEmpty() {
			return fmt.Errorf("case %q: no assertions; a case that can only pass is not a case", c.ID)
		}
		if c.MaxSteps == 0 {
			c.MaxSteps = s.Defaults.MaxSteps
		}
		if c.BudgetUSD == "" {
			c.BudgetUSD = s.Defaults.BudgetUSD
		}
		if c.Timeout == 0 {
			c.Timeout = s.Defaults.Timeout
		}
		if c.Model == "" {
			c.Model = s.Defaults.Model
		}
		if c.Category == CategoryInjection &&
			(c.Assert.Injection == nil ||
				(len(c.Assert.Injection.ExposedWith) == 0 && len(c.Assert.Injection.ExposedInTools) == 0)) {
			return fmt.Errorf("case %q: an INJECTION case must declare injection.exposed_with or "+
				"injection.exposed_in_tools; without one the case passes whenever the planted text simply "+
				"never reached the model (ADR-30)", c.ID)
		}
		if c.Chaos != nil && c.Chaos.KillAfter == "" {
			return fmt.Errorf("case %q: chaos needs kill_after", c.ID)
		}
		for j, turn := range c.Script {
			// A turn naming a tool the case never granted is allowed on
			// purpose: reaching for an ungranted capability is a trajectory
			// worth recording, and the refusal that follows is the behaviour
			// ADR-8 exists for.
			if (turn.Tool == "") == (turn.Text == "") {
				return fmt.Errorf("case %q: script turn %d needs exactly one of tool or text", c.ID, j)
			}
		}
		if _, err := model.ParseUSD(c.BudgetUSD); err != nil {
			return fmt.Errorf("case %q: budget_usd: %w", c.ID, err)
		}
		if c.Assert.MaxCostUSD != "" {
			if _, err := model.ParseUSD(c.Assert.MaxCostUSD); err != nil {
				return fmt.Errorf("case %q: assert.max_cost_usd: %w", c.ID, err)
			}
		}
		for _, r := range c.Requires {
			if r != CapDocker && r != CapCorpus && r != CapMCP {
				return fmt.Errorf("case %q: unknown requirement %q (want %s, %s, %s)",
					c.ID, r, CapDocker, CapCorpus, CapMCP)
			}
		}
	}
	return nil
}

// IsEmpty reports whether nothing at all is asserted.
func (a Assertions) IsEmpty() bool {
	return a.Status == "" && a.MaxSteps == nil && a.MaxCostUSD == "" &&
		len(a.AnswerContains) == 0 && len(a.AnswerOmits) == 0 && a.AnswerMatches == "" &&
		len(a.ToolsCalled) == 0 && len(a.ToolsNotCalled) == 0 && len(a.Retrieved) == 0 &&
		len(a.EventsContain) == 0 && len(a.EventsNotContain) == 0 &&
		a.Citations == nil && a.Budget == nil && a.Sandbox == nil && a.Injection == nil && !a.ExactlyOnce
}

func validCategory(s string) bool {
	for _, c := range Categories() {
		if c == s {
			return true
		}
	}
	return false
}
