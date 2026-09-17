# Tokenizer notes: citations and case names under `to_tsvector('english', …)`

This is a report only. Nothing in the schema, the index, or the query path was changed.

**Setup.** `chunks.tsv` is `to_tsvector('english', content)`. Lexical search (`store.lexicalQuerySQL`) runs the query through the same `to_tsvector`, then ORs the resulting lexemes into a `to_tsquery` ranked by `ts_rank_cd`. A query lexeme can therefore match only if the corpus text produced the same lexeme.

**What was run.** `SELECT alias, token, dictionaries, lexemes FROM ts_debug('english', …)` against the `agentd` database on:

- 5 citation strings, written the way the new cases write them (compact `U.S.C.`)
- 2 of those citations written the way the corpus usually prints them (spaced `U. S. C.`)
- 3 case names from the new `case_name` cases

The chunk counts further down come from `count(*) FILTER (WHERE tsv @@ '<lexeme>'::tsquery)` over all 70,736 chunks. Those are frequency counts only; no retrieval mode was run.

## ts_debug output

Blank tokens are omitted. An empty `lexemes` cell means the token was a stopword and dropped.

### Citations (query form)

**`42 U.S.C. § 1396p(a)`** → `'1396p':3 '42':1 'u.s.c':2`

| alias | token | lexemes |
|---|---|---|
| uint | 42 | {42} |
| file | U.S.C | {u.s.c} |
| numword | 1396p | {1396p} |
| asciiword | a | {} (stopword) |

**`28 U.S.C. § 994(h)`** → `'28':1 '994':3 'h':4 'u.s.c':2`

| alias | token | lexemes |
|---|---|---|
| uint | 28 | {28} |
| file | U.S.C | {u.s.c} |
| uint | 994 | {994} |
| asciiword | h | {h} |

**`11 U.S.C. § 523(a)(2)(A)`** → `'11':1 '2':5 '523':3 'u.s.c':2`

| alias | token | lexemes |
|---|---|---|
| uint | 11 | {11} |
| file | U.S.C | {u.s.c} |
| uint | 523 | {523} |
| asciiword | a | {} (stopword) |
| uint | 2 | {2} |
| asciiword | A | {} (stopword) |

**`328 U.S. 680`** → `'328':1 '680':3 'u.s':2`

| alias | token | lexemes |
|---|---|---|
| uint | 328 | {328} |
| file | U.S | {u.s} |
| uint | 680 | {680} |

**`18 U.S.C. § 3582(c)(2)`** → `'18':1 '2':5 '3582':3 'c':4 'u.s.c':2`

| alias | token | lexemes |
|---|---|---|
| uint | 18 | {18} |
| file | U.S.C | {u.s.c} |
| uint | 3582 | {3582} |
| asciiword | c | {c} |
| uint | 2 | {2} |

### Citations (corpus form)

**`42 U. S. C. § 1396p(a)(l)`** (how Wos v. E.M.A. prints it, OCR `l` included) → `'1396p':5 '42':1 'c':4 'l':7 'u':2`

| alias | token | lexemes |
|---|---|---|
| uint | 42 | {42} |
| asciiword | U | {u} |
| asciiword | S | {} (stopword) |
| asciiword | C | {c} |
| numword | 1396p | {1396p} |
| asciiword | a | {} (stopword) |
| asciiword | l | {l} |

**`328 U. S. 680`** (how IBP v. Alvarez prints it) → `'328':1 '680':4 'u':2`

| alias | token | lexemes |
|---|---|---|
| uint | 328 | {328} |
| asciiword | U | {u} |
| asciiword | S | {} (stopword) |
| uint | 680 | {680} |

### Case names

**`Leng May Ma v. Barber`** → `'barber':5 'leng':1 'ma':3 'may':2 'v':4`

| alias | token | lexemes |
|---|---|---|
| asciiword | Leng | {leng} |
| asciiword | May | {may} |
| asciiword | Ma | {ma} |
| asciiword | v | {v} |
| asciiword | Barber | {barber} |

**`Kwong Hai Chew v. Colding`** → `'chew':3 'cold':5 'hai':2 'kwong':1 'v':4`

| alias | token | lexemes |
|---|---|---|
| asciiword | Kwong | {kwong} |
| asciiword | Hai | {hai} |
| asciiword | Chew | {chew} |
| asciiword | v | {v} |
| asciiword | Colding | **{cold}** (stemmed) |

**`Tennessee Coal, Iron & R. Co. v. Muscoda Local No. 123`** → `'123':10 'co':5 'coal':2 'iron':3 'local':8 'muscoda':7 'r':4 'tennesse':1 'v':6`

| alias | token | lexemes |
|---|---|---|
| asciiword | Tennessee | {tennesse} (stemmed) |
| asciiword | Coal | {coal} |
| asciiword | Iron | {iron} |
| blank | & | (dropped) |
| asciiword | R | {r} |
| asciiword | Co | {co} |
| asciiword | v | {v} |
| asciiword | Muscoda | {muscoda} |
| asciiword | Local | {local} |
| asciiword | No | {} (stopword) |
| uint | 123 | {123} |

## Lexeme frequency in the ingested chunks

| lexeme or text pattern | chunks (of 70,736) |
|---|---|
| text contains `U. S. C.` (spaced) | 11,949 |
| text contains `U.S.C.` (compact) | 765 |
| lexeme `u.s.c` | 738 |
| text contains `U. S.` (spaced) | 40,651 |
| text contains `U.S.` (compact) | 3,122 |
| lexeme `u.s` | 2,602 |
| lexeme `c` | 18,627 |
| lexeme `v` | 34,680 |
| lexeme `2` | 12,350 |
| lexeme `h` | 3,149 |
| lexeme `l` | 4,901 |
| lexeme `may` | 20,025 |
| lexeme `cold` | 116 |
| text contains `Colding` | 5 |
| lexeme `1396p` | 9 |
| lexeme `994` | 189 |
| lexeme `523` | 682 |
| lexeme `680` | 214 |
| lexeme `ma` | 54 |
| lexeme `leng` | 20 |
| lexeme `kwong` | 11 |
| lexeme `muscoda` | 3 |

## What hurts BM25 matching

1. **`U.S.C.` and `U. S. C.` produce different lexemes, and the corpus mostly uses the one agents don't type.**
   - The default parser reads compact `U.S.C` as a single `file` token and keeps it as `u.s.c`.
   - Spaced `U. S. C.` becomes three words: `u`, `s` (a stopword, dropped), and `c`.
   - About 94% of chunks that print a U.S. Code citation use the spaced form (11,949 spaced vs 765 compact). A query like "42 U.S.C. § 1396p(a)" contributes `u.s.c`, which matches only the 738 compact-form chunks.
   - Reporter citations have the same split: `U.S.` → `u.s` (2,602 chunks), `U. S.` → `u` (40,651 chunks contain the spaced text).
   - The title and section numbers still match as bare numbers, so the citation is not lost entirely. But the one token that marks the string as a U.S. Code citation does not line up.

2. **`§` is dropped.** The parser treats it as a blank. Nothing tells a section number apart from any other number, so `994` also matches page cites, dollar amounts, and years.

3. **Subsection letters are dropped or become noise.**
   - `(a)` and `(A)` vanish because `a` is a stopword. `523(a)(2)(A)` reduces to `523` + `2`, and `1396p(a)` to `1396p`. The query can't tell § 523(a)(2)(A) from § 523(a)(2)(B), which Field v. Mans contrasts.
   - `(h)`, `(c)` and `(l)` survive as single-letter lexemes (`h` in 3,149 chunks, `c` in 18,627, `l` in 4,901). The spaced `U. S. C.` also produces `c`, so `c` shows up in over a quarter of all chunks.
   - `(2)` becomes `2` (12,350 chunks).
   - Under the OR-of-lexemes query these add near-universal terms. `ts_rank_cd` rewards terms that appear close together in order. That helps a little here, but the dropped tokens also change positions, so the spacing between terms in the query does not mirror the corpus text.

4. **OCR errors change lexemes.** Wos prints `§ 1396p(a)(l)` with the letter `l`, which becomes the lexeme `l`. A correct `(a)(1)` query would produce `1` instead. The same `l`-for-`1` substitution appears in `§ 1395x(v)(l)(A)(ii)` (Bowen v. Georgetown) and `§ 1681a(k)(l)(B)(i)` (Safeco). The label file notes cover the Wos and Bowen cases.

5. **Alphanumeric section numbers are kept whole, which helps.** `1396p` is a `numword` and survives as one lexeme, found in 9 chunks. It is the most selective token in these citation strings. Purely numeric sections (`994`, `523`, `3582`) are `uint` tokens, also kept whole, but they collide with other numbers.

6. **Case names: stemming collisions and uninformative tokens.**
   - `Colding` → `cold` matches 116 chunks, but only 5 contain "Colding". Postgres stemming turns the party name into a common word.
   - `Tennessee` → `tennesse` is harmless because the corpus stems it the same way. But one corpus citation abbreviates the name as `Tennessee C., I. & R. Co.` (Adams Fruit ¶13). That produces `c` (and `i`, a stopword), not `coal`/`iron`, so the query shares only `tennesse`, `r`, `co`, `muscoda`, `local` and `123` with it.
   - `v` survives as a lexeme in 34,680 chunks (49%), and `r`/`co` are similarly common. They carry almost no signal.
   - `May` is not an English stopword in Postgres. As a lexeme it matches 20,025 chunks, because the auxiliary verb "may" is everywhere in opinions. In "Leng May Ma v. Barber" the discriminating lexemes are `leng` (20 chunks) and `ma` (54).
   - `No.` → `no` is a stopword, so docket-style names ("Local No. 123") lose the marker and keep the bare number.
   - Rare surnames (`kwong` 11, `muscoda` 3, `leng` 20) are what make case-name queries work for BM25. Common-word or stemmed names (`Colding`, `May`, `Barber`) do not.

7. **Hyphens and quotes were not in this sample.** They show up in other new cases, e.g. `§ 2000e-2(m)` and `Rooker-Feldman`, and were not run through `ts_debug`. Postgres splits hyphenated words into the compound plus its parts, so they probably behave better than the patterns above, but that is unverified.

## Categories most exposed

- **`citation`:** items 1 to 4 apply to every U.S. Code query in the set. The labels were written against paragraphs that mostly use the spaced form, so BM25's result on this category reflects this tokenizer mismatch as much as ranking quality.
- **`case_name`:** Kwong Hai Chew (`colding` → `cold`), Tennessee Coal v. Muscoda (the abbreviated form in Adams Fruit), and Leng May Ma (`may`).
