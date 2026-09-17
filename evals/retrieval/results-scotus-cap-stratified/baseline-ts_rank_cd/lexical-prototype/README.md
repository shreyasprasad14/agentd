# Lexical ranking prototype: ts_rank_cd vs Okapi BM25

Generated 2026-09-16 by a throwaway harness (`cmd/lexproto`, since removed) that scored
two lexical rankers with `eval.Run`, unchanged, over `evals/retrieval/generated/stratified-<category>.yaml`.

- `*.current.json` — `store.SearchLexical` as it was at the time: OR-of-lexemes candidates
  ranked by `ts_rank_cd`. On the four categories the eval command had finished
  (paraphrase, citation, case_name, term_of_art) these match the `bm25` row of
  `../stratified-<category>.json` exactly, which validates the harness.
- `*.okapi.json` — the same candidate set ranked by Okapi BM25, k1=1.2, b=0.75
  (textbook defaults, not tuned), with IDF and chunk lengths from TEMP tables built by
  `ts_stat`.

Lexical mode only; no other mode was run. This comparison led to ADR-41. The shipped
implementation (migration 0004, `store.SearchLexical`) was checked against `okapi` and
reproduces it to three decimals in all five categories.

The mixed category has no eval-command baseline: that run was stopped partway when the
ranker changed. `stratified-mixed.current.json` here is the only ts_rank_cd number for it.
