# Stratified retrieval eval: results by category

**Labels:** `evals/retrieval/labels-scotus-cap-stratified.yaml`, 103 cases. The 40 paraphrase cases are copied from `labels-scotus-cap.yaml`; the 63 new ones are **unreviewed drafts**.
**Corpus:** CAP SCOTUS, `data/corpus/scotus-cap-dedup.jsonl`: 2,484 documents, 70,736 chunks.
**Models:** embeddings `mxbai-embed-large`; reranker `qwen2.5:7b` (local, through Ollama).
**Code:** commit `75c8c0d` with an uncommitted working tree. `run-metadata.json` records sha256 hashes of the ranker's source files.
**Run:** 2026-09-16 20:10 UTC to 2026-09-17 04:53 UTC. Interrupted once by a power loss and resumed on identical code (`run-interruption.md`).

> **Read this first: the lexical ranker changed during this task.** The first run showed the `bm25` mode coming last in every
> category. That mode was not BM25: it ranked with Postgres `ts_rank_cd`, which ignores how rare a term is. With your approval it
> was replaced by Okapi BM25 (k1=1.2, b=0.75, not tuned; migration 0004, ADR-41), and **every number in this report is
> measured on the new ranker**. The partial `ts_rank_cd` run is kept in `baseline-ts_rank_cd/` for comparison (§3). Vector
> results are identical under both rankers, as they should be.

## Contents

1. [How it was run](#1-how-it-was-run)
2. [Label mapping checks](#2-label-mapping-checks-step-2)
3. [Before/after the ranker change](#3-beforeafter-the-ranker-change)
4. [Table 1: paragraph/chunk-level labels](#4-table-1-paragraphchunk-level-labels)
5. [Table 2: document-level labels](#5-table-2-document-level-labels)
6. [Best mode per category](#6-best-mode-per-category)
7. [The reranker loses MRR by reordering chunks within the right opinion](#7-the-reranker-loses-mrr-by-reordering-chunks-within-the-right-opinion)
8. [Per-case results (63 new cases)](#8-per-case-results-63-new-cases)
9. [Tokenizer and the lexical (BM25) results](#9-tokenizer-and-the-lexical-bm25-results)
10. [Suspected label issues](#10-suspected-label-issues)
11. [Caveats](#11-caveats)
12. [Files and reproduction](#12-files-and-reproduction)

## 1. How it was run

**Scorer, unchanged** (`internal/retrieval/eval/eval.go`). Each query is searched once per mode at depth 50.
- For each labeled item (a `relevant` entry), the scorer records the rank of the first hit that matches it.
- **recall@k** is the fraction of a case's labeled items found in the top k, averaged over cases. So a case with 5 labeled paragraphs, 3 of them in the top 8, scores 0.6.
- **MRR** is 1 / (rank of the first hit matching *any* label), averaged over cases. It is 0 if nothing matches in the top 50.

**Label conversion** (`cmd/evalstrat convert`, outside the retrieval code):
- A paragraph label becomes one `relevant` entry. Its ordinals are every chunk whose `[char_start, char_end)` overlaps the paragraph's byte span, so recall counts **paragraphs**, not chunks.
- A paragraph is a line of the document text, blank lines skipped (the definition in `stratified_labels_test.go`).
- Document-level variants strip ordinals and keep **one entry per distinct document**, so recall there counts documents.

**Runs:**
- All 11 generated files went through `make eval-retrieval` in all four modes.
- `cmd/evalstrat percase` then re-ran every case in every mode, recorded the top 50 hits, and scored each case alone with the same `eval.Run`.
- **Cross-check:** averaging the per-case scores reproduces every eval-command aggregate exactly (all 10 files, all modes, R@8/R@20/MRR). So the per-case tables (§8) and the aggregates come from the same rankings, and the reranker (temperature 0) gave identical results across runs.

**Beyond the request:** four extra document-level files, one per new category. They give Table 2 per-category rows directly from the eval command. Each matches the corresponding slice of the 63-case document-level file exactly.

## 2. Label mapping checks (Step 2)

All checks passed, so no case was dropped or changed. Details: `evals/retrieval/generated/mapping-checks.json`.

| check | result |
|---|---|
| JSONL text sha256 = `documents.content_sha256` | 161 / 161 documents match |
| source_id present in `documents` | 161 / 161 |
| paragraph ordinal in range | 315 / 315 paragraph labels |
| every paragraph label maps to ≥1 chunk | 315 / 315 (1 chunk: 127, 2 chunks: 167, 3 chunks: 21) |
| evidence excerpt (whitespace-normalized) is in the concatenated content of the mapped chunks | 315 / 315 |
| evidence excerpt is in the paragraph itself (repeats `stratified_labels_test.go`) | 315 / 315 |

## 3. Before/after the ranker change

For context only; the rest of the report uses the BM25 run. The "before" numbers come from the stopped `ts_rank_cd` run.
- **mixed:** that run was cut off before mixed finished, so only its `bm25` row exists, from the lexical-only prototype. That prototype reproduced the eval command's `bm25` rows exactly on the four categories that did finish.
- **vector:** unchanged everywhere, as expected.

| category | n | mode | R@8 before → after | R@20 before → after | MRR before → after |
|---|---:|---|---|---|---|
| paraphrase | 40 | vector | 1.000 → 1.000 | 1.000 → 1.000 | 0.942 → 0.942 |
| paraphrase | 40 | bm25 | 0.850 → 1.000 | 0.850 → 1.000 | 0.608 → 0.927 |
| paraphrase | 40 | hybrid | 0.950 → 1.000 | 1.000 → 1.000 | 0.866 → 0.908 |
| paraphrase | 40 | hybrid+rerank | 1.000 → 1.000 | 1.000 → 1.000 | 0.877 → 0.870 |
| citation | 16 | vector | 0.623 → 0.623 | 0.784 → 0.784 | 0.682 → 0.682 |
| citation | 16 | bm25 | 0.412 → 0.713 | 0.603 → 0.925 | 0.416 → 0.760 |
| citation | 16 | hybrid | 0.593 → 0.601 | 0.748 → 0.884 | 0.677 → 0.776 |
| citation | 16 | hybrid+rerank | 0.647 → 0.712 | 0.831 → 0.881 | 0.750 → 0.802 |
| case_name | 17 | vector | 0.516 → 0.516 | 0.614 → 0.614 | 0.898 → 0.898 |
| case_name | 17 | bm25 | 0.217 → 0.831 | 0.257 → 0.942 | 0.198 → 0.910 |
| case_name | 17 | hybrid | 0.492 → 0.670 | 0.566 → 0.871 | 0.843 → 0.916 |
| case_name | 17 | hybrid+rerank | 0.539 → 0.584 | 0.639 → 0.883 | 0.739 → 0.652 |
| term_of_art | 15 | vector | 0.404 → 0.404 | 0.629 → 0.629 | 0.636 → 0.636 |
| term_of_art | 15 | bm25 | 0.251 → 0.460 | 0.390 → 0.690 | 0.476 → 0.730 |
| term_of_art | 15 | hybrid | 0.370 → 0.423 | 0.630 → 0.656 | 0.643 → 0.735 |
| term_of_art | 15 | hybrid+rerank | 0.552 → 0.531 | 0.701 → 0.747 | 0.724 → 0.607 |
| mixed (bm25 only: eval-command baseline incomplete) | 15 | bm25 | 0.284 → 0.607 | 0.389 → 0.748 | 0.251 → 0.844 |

## 4. Table 1: paragraph/chunk-level labels

Labels as generated. **Paraphrase has only document-level labels.** Its row sits here because that is the only way it is labeled, but it cannot be compared with the rows below it (see Table 2). The `all` row mixes both granularities and is a total, not a comparison.

| category | n | vector R@8 | vector R@20 | vector MRR | bm25 R@8 | bm25 R@20 | bm25 MRR | hybrid R@8 | hybrid R@20 | hybrid MRR | hybrid+rerank R@8 | hybrid+rerank R@20 | hybrid+rerank MRR |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| paraphrase (doc-level labels) | 40 | 1.000 | 1.000 | 0.942 | 1.000 | 1.000 | 0.927 | 1.000 | 1.000 | 0.908 | 1.000 | 1.000 | 0.870 |
| citation | 16 | 0.623 | 0.784 | 0.682 | 0.713 | 0.925 | 0.760 | 0.601 | 0.884 | 0.776 | 0.712 | 0.881 | 0.802 |
| case_name | 17 | 0.516 | 0.614 | 0.898 | 0.831 | 0.942 | 0.910 | 0.670 | 0.871 | 0.916 | 0.584 | 0.883 | 0.652 |
| term_of_art | 15 | 0.404 | 0.629 | 0.636 | 0.460 | 0.690 | 0.730 | 0.423 | 0.656 | 0.735 | 0.531 | 0.747 | 0.607 |
| mixed | 15 | 0.584 | 0.772 | 0.752 | 0.607 | 0.748 | 0.844 | 0.627 | 0.804 | 0.856 | 0.627 | 0.888 | 0.872 |
| all | 103 | 0.714 | 0.816 | 0.822 | 0.792 | 0.897 | 0.858 | 0.745 | 0.882 | 0.856 | 0.764 | 0.909 | 0.785 |

## 5. Table 2: document-level labels

The same cases with paragraph ordinals stripped: a hit on any chunk of a labeled document counts. This is the only table where paraphrase and the other categories are measured at the same granularity.

| category | n | vector R@8 | vector R@20 | vector MRR | bm25 R@8 | bm25 R@20 | bm25 MRR | hybrid R@8 | hybrid R@20 | hybrid MRR | hybrid+rerank R@8 | hybrid+rerank R@20 | hybrid+rerank MRR |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| paraphrase | 40 | 1.000 | 1.000 | 0.942 | 1.000 | 1.000 | 0.927 | 1.000 | 1.000 | 0.908 | 1.000 | 1.000 | 0.870 |
| citation | 16 | 0.875 | 0.938 | 1.000 | 0.906 | 1.000 | 1.000 | 0.906 | 0.969 | 1.000 | 1.000 | 1.000 | 0.969 |
| case_name | 17 | 0.609 | 0.714 | 0.913 | 0.890 | 0.988 | 0.950 | 0.771 | 0.929 | 0.945 | 0.825 | 0.978 | 0.917 |
| term_of_art | 15 | 0.644 | 0.733 | 1.000 | 0.700 | 0.806 | 1.000 | 0.700 | 0.772 | 1.000 | 0.694 | 0.772 | 0.933 |
| mixed | 15 | 0.911 | 0.967 | 0.967 | 0.856 | 0.967 | 1.000 | 0.911 | 0.967 | 0.967 | 0.922 | 0.967 | 0.956 |
| new (4 categories) | 63 | 0.757 | 0.835 | 0.969 | 0.841 | 0.943 | 0.986 | 0.822 | 0.911 | 0.977 | 0.861 | 0.932 | 0.943 |
| all (derived: 40·paraphrase + 63·new, /103) | 103 | 0.851 | 0.899 | 0.958 | 0.903 | 0.965 | 0.963 | 0.891 | 0.945 | 0.951 | 0.915 | 0.958 | 0.915 |

**Label granularity differs between paraphrase and the other categories. Do not compare across tables.**
- Paraphrase's 40 cases were written with document-level labels. The 63 new cases have paragraph-level labels.
- In Table 1, paraphrase's 1.000 recall@8 and the new categories' 0.4–0.8 measure different tasks: "any chunk of the right opinion" vs "the specific paragraphs".
- Compare paraphrase with other categories only in Table 2.
- Even in Table 2 the new categories carry more labeled documents per case (up to 6) than paraphrase (1). Their document-level recall is a stricter test than paraphrase's.

## 6. Best mode per category

**How to read the leads.** "Case-equivalents" is lead × n: how many cases' worth of the metric the gap adds up to. With n = 15–17 per category, a lead under about 1.5 case-equivalents is a difference of one or two cases' partial recall, and is not called meaningful here. "Per case better/worse" counts the cases where the leading mode strictly beats the runner-up on that metric.

**Case hit rates are near saturation.** In 59 of 63 new cases every mode puts **at least one** labeled paragraph in the top 8 (full counts in §8). So recall@8 differences are almost entirely about *how many* of a case's several labeled paragraphs reach the top 8, not whether the case is found.

| category | n | recall@8: best (lead over runner-up) | MRR: best (lead over runner-up) | verdict |
|---|---:|---|---|---|
| paraphrase *(doc-level)* | 40 | four-way tie at 1.000 | vector 0.942 (+0.015 over bm25 = 0.6 case-eq; 5 better / 3 worse) | **No meaningful difference.** Every mode finds every case. Vector's MRR edge is under one case. |
| citation | 16 | bm25 and hybrid+rerank **exact tie** at 0.7125 (the eval command prints 0.713 / 0.712, a floating-point rounding artifact). Both lead vector by 0.090 (1.4 case-eq) and hybrid by 0.111 (1.8 case-eq) | hybrid+rerank 0.802 (+0.026 over hybrid = 0.4 case-eq; 4 better / 4 worse) | **No clear winner.** bm25 and hybrid+rerank are ahead of vector and hybrid on recall@8 by 1–2 cases' worth. MRR is effectively tied among hybrid, hybrid+rerank and bm25; vector trails by 1.9 case-eq. |
| case_name | 17 | **bm25 0.831** (+0.161 over hybrid = 2.7 case-eq; **9 better / 0 worse**). Over vector: +0.315 = 5.4 case-eq | hybrid 0.916 (+0.006 over bm25 = 0.1 case-eq) | **bm25 on recall@8: the one lead in this report that is both large and consistent case by case** (never worse than hybrid in any case). MRR is a three-way tie among hybrid, bm25 and vector; hybrid+rerank trails badly (0.652, §7). |
| term_of_art | 15 | hybrid+rerank 0.531 (+0.070 over bm25 = 1.05 case-eq; 9 better / 5 worse) | hybrid 0.735 (+0.005 over bm25 = 0.08 case-eq) | **No meaningful winner on recall@8** (about one case). MRR: hybrid and bm25 tie; both lead vector by about 1.5 case-eq and hybrid+rerank by 1.9. |
| mixed | 15 | hybrid and hybrid+rerank **exact tie** at 0.627. bm25 0.607, vector 0.584 (spread 0.043 = 0.65 case-eq) | hybrid+rerank 0.872 (+0.017 over hybrid = 0.25 case-eq; 2 better / 2 worse) | **No meaningful difference** on either metric among the four modes, except vector's MRR, which trails by 1.4–1.8 case-eq. |

**Document level (Table 2)** tells the same story, with smaller gaps.
- **case_name:** bm25 leads recall@8 again (0.890 vs 0.825 for hybrid+rerank = 1.1 case-eq; vs vector 0.609 = 4.8 case-eq).
- **citation:** hybrid+rerank reaches 1.000 recall@8 against 0.906 for bm25 and hybrid (1.5 case-eq).
- **Every other category:** document-level leads are one case-equivalent or less.

## 7. The reranker loses MRR by reordering chunks within the right opinion

hybrid+rerank has the lowest paragraph-level MRR on case_name (0.652) and term_of_art (0.607), well under hybrid's 0.916 and 0.735. The rank-1 counts show where it goes:

| category | n | first label at rank 1: vector / bm25 / hybrid / **hybrid+rerank** |
|---|---:|---|
| paraphrase | 40 | 36 / 35 / 34 / **32** |
| citation | 16 | 8 / 9 / 10 / **11** |
| case_name | 17 | 15 / 15 / 15 / **8** |
| term_of_art | 15 | 7 / 9 / 9 / **6** |
| mixed | 15 | 9 / 11 / 11 / **12** |

**What happens in the demoted cases:**
- In 13 new cases, hybrid had a labeled paragraph at rank 1 and the reranker moved it down: 7 to rank 2, the rest to ranks 3–5 (see §8).
- The reranker also promoted a label to rank 1 in 5 cases.
- **In 11 of the 13 demotions, the reranker's new rank-1 chunk comes from a labeled opinion. It is an unlabeled paragraph of the right case, not a wrong case.**
- At document level the reranker finds a relevant opinion at rank 1 in 57 of 63 new cases.

**Two readings, which this data cannot separate:**
- **(a) Reranker error.** The reranker prefers topical passages over the specific holding.
- **(b) Label incompleteness.** Several promoted chunks read as on point (quoted in §10.2), which points to incomplete paragraph labels.

§10.2 lists all 11 for review. Until those are reviewed, the reranker's paragraph-level MRR on case_name and term_of_art should not be read as a defect.

**Cost.** The reranker took 5,904 s for the 103-case file, against 16 s for vector and 98–99 s for bm25 and hybrid.

## 8. Per-case results (63 new cases)

Paragraph-level recall@8 per mode (**bold** = at least one labeled paragraph in the top 8). Rank columns give the first labeled chunk's rank, computed as 1/MRR from the unchanged scorer; "—" means none in the top 50. The last column is the same rank at document level, in the order vector/bm25/hybrid/hybrid+rerank.

**Cases with no labeled paragraph in the top 8** (the others all hit in every mode):
- vector: 53, 59, 73
- bm25: none
- hybrid: 53, 59, 73
- hybrid+rerank: 59, 68

| # | cat | query | labels | vector R@8 | bm25 R@8 | hybrid R@8 | hybrid+rerank R@8 | vector rank | bm25 rank | hybrid rank | hybrid+rerank rank | doc-level rank (v/b/h/r) |
|---:|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 40 | citation | 15 U.S.C. § 1681m(a) when is an insurance rate increase based on a … | 4 | **1.00** | **0.75** | **0.75** | **1.00** | 1 | 2 | 1 | 3 | 1/1/1/1 |
| 41 | citation | 28 U.S.C. § 994(h) maximum term authorized includes statutory recid… | 5 | **0.60** | **0.80** | **1.00** | **1.00** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 42 | citation | 42 U.S.C. § 1396p(a) anti-lien provision and a state's claim on a M… | 5 | **0.80** | **0.60** | **0.60** | **0.40** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 43 | citation | 26 U.S.C. § 6321 federal tax lien on an inheritance the taxpayer di… | 4 | **0.75** | **1.00** | **1.00** | **0.75** | 2 | 3 | 2 | 1 | 1/1/1/1 |
| 44 | citation | 11 U.S.C. § 523(a)(2)(A) what level of reliance must a creditor show | 5 | **0.20** | **0.60** | **0.20** | **0.80** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 45 | citation | 8 U.S.C. § 1231(a)(6) detention of inadmissible aliens beyond the 9… | 4 | **0.75** | **1.00** | **0.75** | **0.75** | 2 | 2 | 2 | 1 | 1/1/1/1 |
| 46 | citation | 28 U.S.C. § 636(b)(1)(B) prisoner petitions challenging conditions … | 5 | **1.00** | **0.80** | **1.00** | **0.60** | 1 | 1 | 1 | 2 | 1/1/1/2 |
| 47 | citation | 11 U.S.C. § 522(l) trustee objection to claimed exemption after 30 … | 5 | **0.40** | **0.60** | **0.60** | **0.60** | 2 | 2 | 2 | 1 | 1/1/1/1 |
| 48 | citation | 29 U.S.C. § 1103(c)(1) anti-inurement provision employer use of pen… | 4 | **0.75** | **1.00** | **0.75** | **1.00** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 49 | citation | 21 U.S.C. § 802(44) felony drug offense state misdemeanor punishabl… | 4 | **0.50** | **0.25** | **0.50** | **0.75** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 50 | citation | 28 U.S.C. § 1915(d) is an in forma pauperis complaint that fails Ru… | 4 | **0.50** | **0.50** | **0.25** | **0.75** | 2 | 1 | 2 | 4 | 1/1/1/1 |
| 51 | citation | 42 U.S.C. § 406(b) contingent-fee agreement versus lodestar for Soc… | 5 | **0.80** | **0.60** | **0.80** | **0.60** | 3 | 2 | 1 | 1 | 1/1/1/1 |
| 52 | citation | 28 U.S.C. § 1367(d) tolling for state-law claims against a nonconse… | 4 | **0.25** | **0.75** | **0.25** | **0.50** | 3 | 3 | 3 | 4 | 1/1/1/1 |
| 53 | citation | 28 U.S.C. § 1500 Court of Federal Claims jurisdiction when a suit o… | 5 | 0.00 | **0.40** | 0.00 | **0.40** | — | 2 | 13 | 2 | 1/1/1/1 |
| 54 | citation | 328 U.S. 680 walking time on the employer's premises before the wor… | 3 | **0.67** | **1.00** | **0.67** | **1.00** | 4 | 1 | 1 | 1 | 1/1/1/1 |
| 55 | citation | 18 U.S.C. § 3582(c)(2) does Booker make the sentence reduction poli… | 4 | **1.00** | **0.75** | **0.50** | **0.50** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 56 | case_name | United States v. Bess federal tax lien attaches consequences to rig… | 3 | **0.67** | **1.00** | **0.67** | **0.33** | 1 | 1 | 1 | 5 | 1/1/1/1 |
| 57 | case_name | Brown v. Felsen consent judgment and whether a bankruptcy court can… | 4 | **0.75** | **0.75** | **0.75** | **0.50** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 58 | case_name | Baxter v. Palmigiano adverse inference from refusal to testify in c… | 7 | **0.43** | **0.71** | **0.71** | **0.57** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 59 | case_name | Garrity v. New Jersey public employees forced to choose between sel… | 3 | 0.00 | **0.33** | 0.00 | 0.00 | 42 | 7 | 14 | 20 | 42/7/14/3 |
| 60 | case_name | United States v. Nardello generic definition of extortion regardles… | 7 | **0.71** | **0.86** | **0.86** | **0.57** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 61 | case_name | Leng May Ma v. Barber alien paroled into the country has not entere… | 5 | **0.60** | **0.80** | **0.60** | **1.00** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 62 | case_name | Myers v. Bethlehem Shipbuilding Corp. exhaustion of administrative … | 4 | **0.50** | **1.00** | **0.75** | **0.75** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 63 | case_name | Tennessee Coal v. Muscoda definition of work under the Fair Labor S… | 3 | **0.33** | **1.00** | **0.67** | **0.33** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 64 | case_name | Hagans v. Lavine jurisdictional questions decided sub silentio are … | 3 | **0.33** | **1.00** | **0.67** | **0.67** | 1 | 1 | 1 | 4 | 1/1/1/4 |
| 65 | case_name | Carlson v. Landon Attorney General discretion to detain deportable … | 8 | **0.38** | **0.62** | **0.50** | **0.38** | 1 | 1 | 1 | 2 | 1/1/1/1 |
| 66 | case_name | Heikkila v. Barber habeas corpus review of deportation orders when … | 6 | **0.33** | **0.83** | **0.50** | **0.50** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 67 | case_name | Lonchar v. Thomas dismissing a first federal habeas petition filed … | 4 | **0.75** | **0.75** | **0.75** | **1.00** | 1 | 1 | 1 | 2 | 1/1/1/1 |
| 68 | case_name | Sims v. Apfel issue exhaustion in a request for Appeals Council review | 5 | **0.40** | **0.80** | **0.80** | 0.00 | 4 | 3 | 2 | 11 | 2/1/1/1 |
| 69 | case_name | United States v. Murdock willfully means a bad purpose in criminal … | 6 | **0.33** | **0.67** | **0.67** | **0.83** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 70 | case_name | Ott v. Mississippi Valley Barge Line fairly apportioned state prope… | 1 | **1.00** | **1.00** | **1.00** | **1.00** | 1 | 1 | 1 | 2 | 1/1/1/1 |
| 71 | case_name | Kwong Hai Chew v. Colding resident alien is a person protected by t… | 4 | **0.75** | **1.00** | **1.00** | **1.00** | 1 | 1 | 1 | 2 | 1/1/1/1 |
| 72 | case_name | Houghton v. Shafer prisoner need not pursue a futile administrative… | 2 | **0.50** | **1.00** | **0.50** | **0.50** | 1 | 1 | 1 | 2 | 1/1/1/1 |
| 73 | term_of_art | does a prisoner who files an untimely grievance satisfy the proper … | 5 | 0.00 | **0.20** | 0.00 | **0.80** | 10 | 4 | 9 | 1 | 1/1/1/1 |
| 74 | term_of_art | can a nonparty be bound by a prior judgment under a theory of virtu… | 5 | **0.20** | **0.60** | **0.40** | **0.20** | 3 | 3 | 2 | 5 | 1/1/1/1 |
| 75 | term_of_art | when does judicial estoppel prevent a party from taking a position … | 6 | **0.67** | **0.67** | **0.67** | **0.50** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 76 | term_of_art | can a federal court remand a suit for damages to state court under … | 5 | **0.60** | **0.80** | **0.60** | **0.60** | 1 | 1 | 1 | 5 | 1/1/1/1 |
| 77 | term_of_art | does the Rooker-Feldman doctrine divest a federal court of jurisdic… | 4 | **0.50** | **0.25** | **0.75** | **0.50** | 1 | 1 | 4 | 2 | 1/1/1/2 |
| 78 | term_of_art | is the in pari delicto defense available against an investor suing … | 5 | **0.40** | **0.40** | **0.40** | **0.60** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 79 | term_of_art | can a lawsuit filed with probable cause lose Noerr antitrust immuni… | 6 | **0.17** | **0.33** | **0.33** | **0.67** | 2 | 1 | 1 | 3 | 1/1/1/1 |
| 80 | term_of_art | must evidence be suppressed when police violate the knock-and-annou… | 4 | **0.25** | **0.75** | **0.25** | **0.25** | 8 | 2 | 3 | 5 | 1/1/1/2 |
| 81 | term_of_art | can a federal tax lien attach to one spouse's interest in property … | 5 | **0.60** | **0.20** | **0.60** | **0.40** | 3 | 1 | 1 | 3 | 1/1/1/1 |
| 82 | term_of_art | when is an abbreviated quick look analysis enough to condemn a prof… | 6 | **0.50** | **0.50** | **0.33** | **0.67** | 2 | 3 | 1 | 1 | 1/1/1/1 |
| 83 | term_of_art | may a sentencing court use the modified categorical approach when t… | 5 | **0.60** | **0.80** | **0.60** | **0.80** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 84 | term_of_art | does the plain statement rule keep the ADEA from reaching a state c… | 6 | **0.33** | **0.17** | **0.17** | **0.33** | 2 | 1 | 2 | 3 | 1/1/1/1 |
| 85 | term_of_art | can a prior fraud judgment have collateral estoppel effect in a ban… | 6 | **0.67** | **0.67** | **0.67** | **0.50** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 86 | term_of_art | does due process allow res judicata to bar taxpayers who were not p… | 7 | **0.29** | **0.14** | **0.29** | **0.57** | 7 | 3 | 3 | 2 | 1/1/1/1 |
| 87 | term_of_art | can a habeas lawyer's egregious misconduct justify equitable tollin… | 7 | **0.29** | **0.43** | **0.29** | **0.57** | 1 | 5 | 1 | 2 | 1/1/1/1 |
| 88 | mixed | can a civil rights plaintiff who recovers only nominal damages be d… | 6 | **0.33** | **0.50** | **0.50** | **0.83** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 89 | mixed | after Coy v. Iowa, may a child abuse victim testify by one-way clos… | 7 | **0.43** | **0.43** | **0.29** | **0.43** | 5 | 1 | 1 | 1 | 1/1/1/1 |
| 90 | mixed | are Miranda v. Arizona warnings adequate if police say an appointed… | 6 | **0.50** | **0.33** | **0.50** | **0.83** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 91 | mixed | under Strickland v. Washington what prejudice must a defendant show… | 6 | **0.67** | **0.50** | **0.67** | **0.67** | 1 | 2 | 1 | 1 | 1/1/1/1 |
| 92 | mixed | is a state marijuana distribution conviction that could cover shari… | 5 | **0.80** | **0.80** | **0.60** | **0.40** | 2 | 1 | 1 | 1 | 1/1/1/1 |
| 93 | mixed | does 42 U.S.C. § 2000e-2(m) require direct evidence of discriminati… | 6 | **0.50** | **0.50** | **0.50** | **0.83** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 94 | mixed | can a lodestar fee award under 42 U.S.C. § 1988 be enhanced because… | 5 | **0.60** | **0.80** | **0.80** | **0.80** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 95 | mixed | may EPA weigh costs against benefits when setting best technology a… | 5 | **0.60** | **0.40** | **0.40** | **0.80** | 4 | 3 | 3 | 2 | 1/1/1/1 |
| 96 | mixed | can parents get tuition reimbursement under IDEA for a private scho… | 5 | **0.60** | **0.80** | **0.80** | **0.60** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 97 | mixed | given Ohralik v. Ohio State Bar Assn., may a state flatly ban lawye… | 6 | **0.50** | **0.50** | **0.50** | **0.17** | 2 | 1 | 2 | 4 | 2/1/2/1 |
| 98 | mixed | does 42 U.S.C. § 1395x(v)(1)(A)(ii) let HHS reissue Medicare cost-l… | 6 | **0.83** | **0.83** | **0.83** | **0.33** | 2 | 3 | 2 | 3 | 1/1/1/3 |
| 99 | mixed | after Hodel v. Irving, does the 1984 amended escheat provision of t… | 6 | **0.83** | **0.83** | **1.00** | **0.67** | 3 | 2 | 2 | 1 | 1/1/1/1 |
| 100 | mixed | how should a federal court in a diversity case apply New York CPLR … | 6 | **0.67** | **0.67** | **0.67** | **0.67** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 101 | mixed | under United States v. Place, does walking a drug-detection dog aro… | 7 | **0.57** | **0.71** | **0.86** | **0.71** | 1 | 1 | 1 | 1 | 1/1/1/1 |
| 102 | mixed | under Malley v. Briggs, are officers who executed an overbroad warr… | 6 | **0.33** | **0.50** | **0.50** | **0.67** | 1 | 1 | 1 | 1 | 1/1/1/1 |

## 9. Tokenizer and the lexical (BM25) results

`TOKENIZER_NOTES.md` predicted lexical misses on citation and case_name queries. The findings below are **hypotheses from inspecting lexemes and paragraph text**; no fix was tested.

**The ranker, not the tokenizer, caused almost all of the original lexical failure.**
- Under `ts_rank_cd`, lexical recall@8 was 0.412 (citation) and 0.217 (case_name).
- Replacing only the ranking, with the tokenizer untouched, raised them to 0.713 and 0.831 (§3).
- Whatever the tokenizer costs, it was not the main cause.

**Under BM25, no citation or case_name case is a BM25 miss at @8.** bm25 puts at least one labeled paragraph in the top 8 in all 33 cases. What remains are **36 labeled paragraphs (21 citation, 15 case_name) that BM25 ranks below 8** in cases it otherwise hits. Checked against the notes' patterns:

| pattern (`TOKENIZER_NOTES.md`) | what the data shows |
|---|---|
| **Spaced `U. S. C.` vs compact `U.S.C.`** | **Systematic, but it does not single out cases.** The query lexeme `u.s.c` appears in **none** of the labeled paragraphs' chunks in any citation case, whether BM25 found the paragraph or missed it, because the corpus prints the spaced form. Every citation query therefore carries one lexeme that never matches a label. That plausibly lowers all citation labels' BM25 scores a little. It cannot explain why a particular paragraph was missed, because the found ones lack it too. |
| **§ dropped; subsection letters dropped or noisy** | **Not a visible cause of any miss.** The section number itself (`1396p`, `1231`, `1367`, `1500`, `3582`, `522`, `523`) survives, and **18 of the 21 missed citation paragraphs contain it**. Where it is absent (case 40 ¶33, 46 ¶16, 49 ¶2), **the paragraph does not cite the section at all**. Case 49 ¶2 cites § 841(b)(1)(A), not § 802(44). Those are wording misses, not tokenization. |
| **OCR `(l)` for `(1)`** | **Present, minor.** Case 98 (mixed): Bowen ¶28 prints `1395x(v)(l)(A)(ii)` → lexemes `1395x v l ii`; the query gives `1395x v 1 ii`. Only the common `1`/`l` token differs, the distinctive `1395x` matches, and BM25 still ranks that label 3rd. Case 42 (Wos `1396p(a)(l)`) avoided the issue because the query was written as `§ 1396p(a)`. |
| **`Colding` → `cold`** | **Not borne out.** Case 71 (Kwong Hai Chew v. Colding): bm25 recall@8 1.00, first label at rank 1. The rare lexemes `kwong` (11 chunks), `hai`, `chew` carry the query, and BM25's IDF gives the stemmed `cold` little weight. |
| **`May` as a common lexeme (Leng May Ma)** | **Possible, weak.** Case 61: bm25 recall@8 0.80. The one missed paragraph (Sale ¶37) sits at rank 9, just outside. It cites the case in short form without `barber`, keeping only `leng`/`ma` of the name. With IDF, `may` (20,025 chunks) carries little weight, so a short-form citation is the likelier cause. |
| **Tennessee Coal abbreviated in Adams Fruit** | **Not borne out.** Case 63: bm25 recall@8 1.00. |
| **Stemming, e.g. `willfully` → `will`** | **Not a cause.** Case 69 ¶66 does not contain "willful" in any form. Postgres stems willfully/willful/willfulness identically. |

**Cases where the tokenizer, rather than the ranker, plausibly explains a BM25 miss:**
- **Paragraph level:** none are clear-cut.
- **Weak candidates:** case 61 (Sale ¶37, rank 9) and the uniform `u.s.c` handicap on all 16 citation cases.
- **The rest:** every other missed paragraph lacks the query's content words or the citation itself. That is a query–paragraph wording gap. Vector search closes part of it: it gets **10 of the 36** into its own top 8 (for example case 42's Ahlborn ¶38 at rank 1 and Wos ¶14 at rank 2). The other 26 neither mode reaches.

Hypotheses worth testing separately, not tested here:
- normalizing `U.S.C.` / `U. S. C.` in queries or at index time;
- mapping OCR `l` to `1` in citation contexts.

**The Garrity case (59) is not a tokenizer problem.** It is among the hardest cases for every mode (first label at rank 42 / 7 / 14 / 20).
- **The predicted trap didn't fire.** The notes warned of a "Garrity rule" trap in Mastrobuono (an arbitration case); BM25 did not fall into it.
- **What outranked the labels were topical neighbors:** Balsys ¶26–27 (Murphy v. Waterfront Commission: New Jersey, self-incrimination, immunity) and Chavez v. Martinez ¶19 (compelled testimony). Neither names Garrity, so under the case_name labeling rule both are correctly unlabeled.
- **The labeled paragraphs** (McKune ¶32, Salinas ¶10 and ¶57) name Garrity but share few other query words.

## 10. Suspected label issues

Nothing below was changed. Each item quotes the chunk as retrieved: the text the rankers saw, which begins with the previous chunk's last sentence as overlap. Paragraph numbers are the paragraphs the chunk's body covers.

### 10.1 Unlabeled chunk ranked above every label in all four modes

The rule you set. Five cases qualify: an unlabeled chunk in the top 8 of every mode, above that mode's first labeled chunk.

**Case 52 [citation]:** *28 U.S.C. § 1367(d) tolling for state-law claims against a nonconsenting State dismissed on Eleventh Amendment grounds*
- **Labels:** Jinks ¶30; Raygor ¶2, ¶27, ¶33. First label at rank 3 / 3 / 3 / 4.
- **Above them, same document, Raygor chunk 12 (¶20–22),** ranked 2 / 1 / 2 / 1. **¶20 states the query's question almost verbatim:**
  > "Even so, there remains the question whether § 1367(d) tolls the statute of limitations for claims against nonconsenting States that are asserted under § 1367(a) but subsequently dismissed on Eleventh Amendment grounds."
- **Also above them, Raygor chunk 19 (¶28–29),** ranked 1 / 2 / 1 / 3. It is the reasoning on the same point:
  > "Given that particular context, it is unclear if the tolling provision was meant to apply to dismissals for reasons unmentioned by the statute, such as dismissals on Eleventh Amendment grounds."
- **Notes:** mention neither ¶20–22 nor ¶28–29. **Likely missing labels.**

**Case 95 [mixed]:** *may EPA weigh costs against benefits when setting best technology available standards … under 33 U.S.C. § 1326(b)*
- **Labels:** Entergy ¶2, ¶25, ¶26, ¶30, ¶32. First label at rank 4 / 3 / 3 / 2.
- **Above them, same document, Entergy chunk 9 (¶14–16),** ranked 2 / 2 / 2 / 1. ¶14 contains the question on which the Court granted certiorari:
  > "We then granted certiorari limited to the following question: 'Whether [§ 1326(b)] . . . authorizes the [EPA] to compare costs with benefits in determining "the best technology available for minimizing adverse environmental impact" at cooling water intake structures.'"
- **¶16 continues:**
  > "the EPA relied on its view that §1326(b)'s 'best technology available' standard permits consideration of the technology's costs … and of the relationship between those costs and the environmental benefits produced"
- **Notes:** do not mention ¶14–16. **Likely missing label.**

**Case 68 [case_name]:** *Sims v. Apfel issue exhaustion in a request for Appeals Council review*
- **Labels:** Woodford ¶65; Sims ¶2, ¶18, ¶22, ¶23. First label at rank 4 / 3 / 2 / 11.
- **Above them, same document, Sims chunk 4 (¶10–12),** ranked 2 / 2 / 1 / 2:
  > "The Commissioner rightly concedes that petitioner exhausted administrative remedies by requesting review by the Council. … Nevertheless, the Commissioner contends that we should require issue exhaustion in addition to exhaustion of remedies."
- **Notes:** ¶12 frames the exact issue. The notes name only Woodford ¶27 and Henderson ¶27 as left out. **Possible missing label**, a framing paragraph rather than the holding.

**Case 86 [term_of_art]:** *does due process allow res judicata to bar taxpayers who were not parties to an earlier suit against the same county tax*
- **Labels:** include South Central Bell ¶21. First label at rank 7 / 3 / 3 / 2.
- **Above them, same document, South Central Bell chunk 9 (¶22–23),** ranked 2 / 1 / 2 / 1:
  > "we considered an Alabama Supreme Court holding that state-law principles of res judicata prevented certain taxpayers from bringing a case … We held that the Fourteenth Amendment forbade this 'extreme' application of state-law preclusion (res judicata) principles … because the plaintiffs in Case Two were 'strangers' to the earlier judgment"
- **Notes:** ¶21 is labeled as "partially relevant". ¶22–23 is the continuation that actually states Richards's holding. The paragraph split looks like an OCR page break (¶23 begins mid-sentence: "case before us."). **Likely a label boundary issue: ¶22–23 belong with ¶21.**

**Case 59 [case_name]:** *Garrity v. New Jersey public employees forced to choose between self-incrimination and losing their jobs*
- **Labels:** McKune ¶32; Salinas ¶10, ¶57. First label at rank 42 / 7 / 14 / 20.
- **Above them, another document, United States v. Balsys chunk 19 (¶26–27),** ranked 1 / 1 / 1 / 2:
  > "When the witnesses persisted in refusing to testify based on their fear of federal prosecution, they were held in civil contempt, and the order was affirmed by New Jersey's highest court."
- **Probably not a label issue.** The passage describes Murphy v. Waterfront Commission, not Garrity, and does not name Garrity, which the case_name rule requires. It is flagged because it meets the rule. The real question for review is whether the query is answerable from the three labeled paragraphs at all (§9).

### 10.2 The reranker's rank-1 chunk in the 11 demotions from §7

These fall outside the "every mode" rule: they are specific to the reranker. They are listed because they account for most of its MRR loss. In each, hybrid had a label at rank 1, and the reranker put this unlabeled chunk of a labeled document first.

| case | reranker's #1 chunk (paragraphs) | excerpt | notes already addressed it? |
|---|---|---|---|
| 40 citation | Safeco chunk 1 (¶5–6) | "The Act requires … that 'any person [who] takes any adverse action with respect to any consumer that is based in whole or in part on any information contained in a consumer report' must notify the affected consumer. 15 U. S. C. § 1681m(a). … 'adverse action' is 'a denial or cancellation of, an increase in any charge for …'" | No. States the provision and the rate-increase definition. **Possible partial label.** |
| 56 case_name | Drye chunk 17 (¶38–39) | ¶39: "In this sense Aquilino follows Bess in requiring that the taxpayer must have a beneficial interest …" | **Contradicts the notes.** They say "every paragraph in the corpus that names Bess is labeled". A corpus scan finds **6** such paragraphs and 3 labeled: Craft ¶12 and Drye ¶3, ¶37 are labeled; **Drye ¶19, ¶23 and ¶39 are not**. All three cite it in short form ("Bess, 357 U. S."), so the labeling scan probably searched for the full name. **Likely missing labels.** |
| 65 case_name | Reno v. Flores chunk 35 (¶42–44) | "Title 8 U. S. C. §1252(a)(1) … provides: '[A]ny such alien taken into custody may, in the discretion of the Attorney General … be continued in custody'" | No. Does not name Carlson; states the statute Carlson construed. **Weak.** Probably correctly unlabeled under the case_name rule. |
| 67 case_name | Lonchar chunk 1 (¶4–6) | "He filed this 'eleventh hour' petition for habeas corpus — his first federal habeas corpus petition — on June 28, 1995, the day of his scheduled execution." | No. Matches the query's own words; it's background rather than the holding. **Possible partial label.** |
| 70 case_name | Polar Tankers chunk 14 (¶27) | "in order to fund services by taxing ships, a State must also impose similar taxes upon other businesses" | Yes, implicitly. The notes say ¶38 is the only paragraph naming Ott; ¶27 does not. **Correctly unlabeled under the case_name rule.** |
| 71 case_name | Zadvydas chunk 26 (¶45–46) | "Hence we leave no 'unprotected spot in the Nation's armor.' Kwong Hai Chew, 344 U. S., at 602." | **Yes.** The notes considered ¶45 and rejected it as an unrelated point. The reranker disagrees with a deliberate call. |
| 72 case_name | McCarthy v. Bronson chunk 6 (¶15) | "Houghton v. Shafer, 392 U. S. 639 (1968); … In Houghton, the prisoner's contention was that prison authorities had violated …" | **Yes.** The notes rejected ¶15 as a block quote listing Houghton. It goes on to describe Houghton's claim, so the reviewer may want to revisit. |
| 76 term_of_art | Quackenbush chunk 37 (¶44–45) | "We ultimately held that Burford did not provide proper grounds for an abstention-based dismissal in NOPSI because the 'case [did] not involve a state-law claim …'" | No. Burford analysis in the same opinion. **Possible partial label.** |
| 79 term_of_art | PRE chunk 17 (¶28) | "Because the absence of probable cause is an essential element of the tort, the existence of probable cause is an absolute defense." | No. Directly addresses "filed with probable cause". **Possible partial label.** |
| 81 term_of_art | Craft chunk 21 (¶33) | "legislative history indicates that Congress did not intend that a federal tax lien should attach to such an interest [in entireties property]" | Partly. The notes rejected ¶19–21, not ¶33. **Possible partial label** (the argument the Court rejects). |
| 87 term_of_art | Holland chunk 16 (¶70–73) | "equitable tolling could not be applied in a case … that involves no more than '[p]ure professional negligence' on the part of a petitioner's attorney" | Near. The notes flag ¶91 as possibly partial; ¶70–73 is the lower-court rule the Court reversed. **Possible partial label.** |

## 11. Caveats

- **Draft labels.** All 63 new cases are unreviewed drafts written by a language model. §10 lists concrete cases where labels look incomplete; several of them affect paragraph-level MRR for the reranker specifically. Every per-category number moves if those are resolved.
- **Labels found by exact-term scanning.** Citation, case_name and term_of_art labels were found by scanning the raw text for the identifier or term. That can favor lexical retrieval, and it can miss paragraphs that discuss the concept without the identifier. case 56 shows the second failure: short-form citations were missed. Two things limit this bias here:
  - bm25 led clearly only on case_name recall@8;
  - on term_of_art, the category most exposed to the bias, bm25 did not beat hybrid+rerank on recall@8 or hybrid on MRR.
- **Queries written by a language model**, as were the original 40. They may echo their source opinions' wording, which ADR-40 already identified as making retrieval easy.
- **Small n.** 15–17 cases per new category. Only differences above about 1.5 case-equivalents are called out, and only case_name bm25 recall@8 is also consistent case by case.
- **Mixed granularity.** Paraphrase is document-level only. Table 1's paraphrase row and `all` row are not comparable with the other rows.
- **The ranker changed mid-task** (see the note at the top). These are the first stratified numbers on BM25. The `ts_rank_cd` numbers in §3 are for comparison and are complete only for four of five categories.
- **The README results table and its prose on why fusion loses to vector are stale.** They describe `ts_rank_cd`. Per your instruction they were not edited.

## 12. Files and reproduction

```
evals/retrieval/generated/                      # generated label files (headers say how)
  stratified-{paraphrase,citation,case_name,term_of_art,mixed}.yaml
  stratified-all.yaml                           # 103 cases
  stratified-new-doclevel.yaml                  # 63 new cases, ordinals stripped
  stratified-{citation,case_name,term_of_art,mixed}-doclevel.yaml
  cases.json                                    # per-label provenance: paragraph spans, chunk ordinals, evidence
  mapping-checks.json                           # §2
evals/retrieval/results-scotus-cap-stratified/
  stratified-*.json                             # raw eval-command output, one per label file
  percase.json                                  # every case × mode: per-case scores and top-50 hits
  run-metadata.json                             # commit, dirty flag, ranker file hashes, migrations, lexical stats, Ollama models
  run-interruption.md
  logs/
  baseline-ts_rank_cd/                          # the stopped pre-change run, plus the lexical prototype comparison
```

```sh
go run ./cmd/evalstrat convert                  # needs the ingested corpus in Postgres and the JSONL
for f in paraphrase citation case_name term_of_art mixed all new-doclevel \
         citation-doclevel case_name-doclevel term_of_art-doclevel mixed-doclevel; do
  make eval-retrieval LABELS=evals/retrieval/generated/stratified-$f.yaml \
                      RESULTS=evals/retrieval/results-scotus-cap-stratified/stratified-$f.json
done
go run ./cmd/evalstrat percase
```

Full run time is about 8 hours on this laptop, nearly all of it in hybrid+rerank.
