package runtime_test

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/retrieval"
	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/tools/builtin"
	"github.com/shreyasprasad/agentd/internal/tools/corpus"
)

// ingestFixtureCorpus loads the shared fixture into the test store with the
// fake embedder, and registers the corpus tools the way `agentd work` does.
func ingestFixtureCorpus(t *testing.T, f *fixture) {
	t.Helper()
	file, err := os.Open("../../evals/retrieval/fixture.jsonl")
	require.NoError(t, err)
	defer file.Close()
	docs, err := retrieval.ReadJSONL(file)
	require.NoError(t, err)

	emb := embed.NewFake()
	sum, err := retrieval.Ingest(context.Background(), f.st, emb, docs, retrieval.IngestOptions{})
	require.NoError(t, err)
	require.Equal(t, len(docs), sum.Ingested)

	searcher := &retrieval.Searcher{Store: f.st, Embedder: emb}
	f.registry.MustRegister(corpus.NewSearch(searcher), corpus.NewFetch(f.st))
}

// TestLoopLegalResearchTrajectory drives the M3 exit-criterion trajectory
// through the real loop: search_corpus, then fetch_document on the found
// case, then finish with a citation, and the cited (source_id, ordinal)
// must exist in the chunks table.
func TestLoopLegalResearchTrajectory(t *testing.T) {
	f := newFixture(t, fake.New(
		fake.ToolUse("t1", corpus.SearchName, map[string]any{
			"query": "warrant for historical cell site location records",
		}, usage),
		fake.ToolUse("t2", corpus.FetchName, map[string]any{
			"source_id": "clop-0002",
		}, usage),
		finishCall("t3", "The Court held that accessing seven days or more of historical cell-site records "+
			"requires a warrant (clop-0002 ¶0)."),
	))
	ingestFixtureCorpus(t, f)

	toolset := []string{corpus.SearchName, corpus.FetchName, builtin.FinishName}
	id := f.submit("What did the Court hold about warrants for cell phone location data? Cite the opinion and paragraph.",
		runOpts{tools: toolset})
	defer f.startWorker("w1", lease)()
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	// The search tool's result carries real chunk and document ids.
	var searchOut corpus.SearchResult
	var fetchOut corpus.FetchResult
	for _, ev := range f.events(id) {
		if ev.Type != runtime.EventToolSucceeded {
			continue
		}
		var p runtime.ToolSucceededPayload
		require.NoError(t, json.Unmarshal(ev.Payload, &p))
		switch p.Name {
		case corpus.SearchName:
			require.NoError(t, json.Unmarshal(p.Result, &searchOut))
		case corpus.FetchName:
			require.NoError(t, json.Unmarshal(p.Result, &fetchOut))
		}
	}
	require.NotEmpty(t, searchOut.Hits, "search_corpus returned hits")
	require.Equal(t, "clop-0002", searchOut.Hits[0].SourceID, "the Carpenter fixture should rank first for this query")
	top := searchOut.Hits[0]
	docID, err := uuid.Parse(top.DocumentID)
	require.NoError(t, err)
	chunks, err := f.st.ListChunks(context.Background(), docID, 0, -1)
	require.NoError(t, err)
	found := false
	for _, c := range chunks {
		if c.ID.String() == top.ChunkID && c.Ordinal == top.Ordinal {
			found = true
		}
	}
	require.True(t, found, "the hit's chunk_id and ordinal exist in the chunks table")

	require.Equal(t, "clop-0002", fetchOut.Document.SourceID)
	require.NotEmpty(t, fetchOut.Chunks)

	// The second model request saw the search result inside the envelope.
	reqs := f.provider.Requests()
	require.Len(t, reqs, 3)
	lastMsg := reqs[1].Messages[len(reqs[1].Messages)-1]
	require.Len(t, lastMsg.Content, 1)
	envelope := lastMsg.Content[0].Content
	require.Contains(t, envelope, `<tool_result tool="search_corpus"`)
	require.Contains(t, envelope, "clop-0002")

	// The final answer's citation resolves to a real chunk.
	state := f.state(id)
	sourceID, ordinal := parseCitation(t, state.FinalAnswer)
	doc, err := f.st.GetDocumentBySourceID(context.Background(), sourceID)
	require.NoError(t, err, "cited source_id exists")
	cited, err := f.st.ListChunks(context.Background(), doc.ID, ordinal, ordinal)
	require.NoError(t, err)
	require.Len(t, cited, 1, "cited ordinal exists")
}

var citationRE = regexp.MustCompile(`\((clop-[0-9a-z-]+) ¶(\d+)\)`)

func parseCitation(t *testing.T, answer string) (string, int) {
	t.Helper()
	m := citationRE.FindStringSubmatch(answer)
	require.NotNil(t, m, "final answer %q must contain a (source_id ¶ordinal) citation", answer)
	n, err := strconv.Atoi(m[2])
	require.NoError(t, err)
	return m[1], n
}

// TestLoopCorpusToolNotAllowlisted: a run granted only finish gets a
// non-retryable tool_failed when the model reaches for search_corpus, and
// the model sees the refusal as an error result.
func TestLoopCorpusToolNotAllowlisted(t *testing.T) {
	f := newFixture(t, fake.New(
		fake.ToolUse("t1", corpus.SearchName, map[string]any{"query": "anything at all"}, usage),
		finishCall("t2", "I cannot search; the tool is not available."),
	))
	ingestFixtureCorpus(t, f)

	id := f.submit("Try to search.", runOpts{tools: []string{builtin.FinishName}})
	defer f.startWorker("w1", lease)()
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	var failed *runtime.ToolFailedPayload
	for _, ev := range f.events(id) {
		if ev.Type == runtime.EventToolFailed {
			var p runtime.ToolFailedPayload
			require.NoError(t, json.Unmarshal(ev.Payload, &p))
			failed = &p
		}
	}
	require.NotNil(t, failed, "the disallowed call must be recorded as tool_failed")
	require.Equal(t, corpus.SearchName, failed.Name)
	require.False(t, failed.Retryable, "an allowlist refusal is not retryable")
	require.Contains(t, failed.Error, "not allowed")

	// The model was told, inside the envelope, as an error.
	reqs := f.provider.Requests()
	require.Len(t, reqs, 2)
	raw, err := json.Marshal(reqs[1].Messages)
	require.NoError(t, err)
	require.Contains(t, string(raw), "not allowed")
}

// TestLoopSearchToolFailureIsRetryable: an unreachable embedder fails the
// call as a tool error the model can react to, not a crashed run.
func TestLoopSearchUnavailableFailsTool(t *testing.T) {
	f := newFixture(t, fake.New(
		fake.ToolUse("t1", corpus.SearchName, map[string]any{"query": "anything at all"}, usage),
		finishCall("t2", "Search is down."),
	))
	// Register the tool with nil deps, the serve-process shape.
	f.registry.MustRegister(corpus.NewSearch(nil), corpus.NewFetch(nil))

	id := f.submit("Try to search.", runOpts{tools: []string{corpus.SearchName, builtin.FinishName}})
	defer f.startWorker("w1", lease)()
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	var sawRetryableFailure bool
	for _, ev := range f.events(id) {
		if ev.Type == runtime.EventToolFailed {
			var p runtime.ToolFailedPayload
			require.NoError(t, json.Unmarshal(ev.Payload, &p))
			require.True(t, p.Retryable, "an unavailable dependency is retryable, unlike an allowlist refusal")
			sawRetryableFailure = true
		}
	}
	require.True(t, sawRetryableFailure)
}

// lease is the worker lease used by these tests; long enough that no
// hand-off happens mid-test.
const lease = 30 * time.Second
