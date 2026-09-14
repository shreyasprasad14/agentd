package evals_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/evals"
)

func read(t *testing.T, yaml string) (*evals.Suite, error) {
	t.Helper()
	return evals.ReadSuite(strings.NewReader(yaml), "test.yaml")
}

const minimalCase = `
cases:
  - id: one
    category: SMOKE
    goal: do a thing
    tools: [finish]
    assert:
      status: succeeded
`

func TestSuiteAppliesDefaults(t *testing.T) {
	s, err := read(t, `
defaults:
  budget_usd: "0.25"
  max_steps: 4
  timeout: 45s
  model: claude-opus-5
`+minimalCase)
	require.NoError(t, err)
	c := s.Cases[0]
	require.Equal(t, "0.25", c.BudgetUSD)
	require.Equal(t, int32(4), c.MaxSteps)
	require.Equal(t, 45*time.Second, c.Timeout)
	require.Equal(t, "claude-opus-5", c.Model)
}

// TestSuiteRejectsAnUnknownAssertion is the reason KnownFields is on. A
// mistyped assertion that silently did not run would be a case that can only
// pass, which is the one thing an eval must never be.
func TestSuiteRejectsAnUnknownAssertion(t *testing.T) {
	_, err := read(t, `
cases:
  - id: one
    category: SMOKE
    goal: do a thing
    tools: [finish]
    assert:
      answer_contain: ["typo"]
`)
	require.ErrorContains(t, err, "answer_contain")
}

func TestSuiteRejectsACaseThatAssertsNothing(t *testing.T) {
	_, err := read(t, `
cases:
  - id: one
    category: SMOKE
    goal: do a thing
    tools: [finish]
    assert: {}
`)
	require.ErrorContains(t, err, "a case that can only pass is not a case")
}

func TestSuiteRejectsAnUnknownCategory(t *testing.T) {
	_, err := read(t, `
cases:
  - id: one
    category: SMOKEE
    goal: do a thing
    tools: [finish]
    assert: {status: succeeded}
`)
	require.ErrorContains(t, err, "unknown category")
}

func TestSuiteRejectsAnUnknownThresholdMetric(t *testing.T) {
	_, err := read(t, `
thresholds:
  SMOKE: {pass_rare: 1.0}
`+minimalCase)
	require.ErrorContains(t, err, "unknown metric")
}

// TestSuiteRequiresInjectionExposure is ADR-30 enforced at parse time. Without
// exposed_with, an injection case passes whenever retrieval simply missed the
// poisoned document — which is the default failure mode of the category, not
// an edge case.
func TestSuiteRequiresInjectionExposure(t *testing.T) {
	_, err := read(t, `
cases:
  - id: one
    category: INJECTION
    goal: do a thing
    tools: [search_corpus, finish]
    assert:
      status: succeeded
      injection:
        canary_absent: ["PWNED"]
`)
	require.ErrorContains(t, err, "exposed_with")
}

func TestSuiteRejectsDuplicateCaseIDs(t *testing.T) {
	_, err := read(t, `
cases:
  - id: one
    category: SMOKE
    goal: a
    tools: [finish]
    assert: {status: succeeded}
  - id: one
    category: SMOKE
    goal: b
    tools: [finish]
    assert: {status: succeeded}
`)
	require.ErrorContains(t, err, "duplicate case id")
}

func TestSuiteRejectsAMalformedScriptTurn(t *testing.T) {
	_, err := read(t, `
cases:
  - id: one
    category: SMOKE
    goal: a
    tools: [finish]
    script:
      - tool: finish
        text: "both at once"
    assert: {status: succeeded}
`)
	require.ErrorContains(t, err, "exactly one of tool or text")
}

func TestSuiteRejectsABadBudget(t *testing.T) {
	_, err := read(t, `
cases:
  - id: one
    category: SMOKE
    goal: a
    tools: [finish]
    budget_usd: "not a number"
    assert: {status: succeeded}
`)
	require.ErrorContains(t, err, "budget_usd")
}

func TestSuiteRejectsAnUnknownRequirement(t *testing.T) {
	_, err := read(t, `
cases:
  - id: one
    category: SMOKE
    goal: a
    tools: [finish]
    requires: [gpu]
    assert: {status: succeeded}
`)
	require.ErrorContains(t, err, "unknown requirement")
}

// TestCheckedInSuitesParse is the guard that keeps the shipped fixtures
// honest: every rule above applies to evals/cases/ too.
func TestCheckedInSuitesParse(t *testing.T) {
	suites, err := evals.LoadSuites("../../evals/cases")
	require.NoError(t, err)
	require.NotEmpty(t, suites)

	seen := map[string]bool{}
	for _, s := range suites {
		for _, c := range s.Cases {
			seen[c.Category] = true
			require.NotEmpty(t, c.Script, "case %q has no script, so its cassette cannot be regenerated", c.ID)
		}
	}
	// Spec §12 names seven categories and the milestone's exit criteria ask
	// for all of them.
	for _, cat := range evals.Categories() {
		require.True(t, seen[cat], "no case in category %s", cat)
	}
}
