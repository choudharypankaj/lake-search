<h1 align="center">lake-search</h1>

<p align="center">
  <strong>Lucene-style search syntax, compiled to Databend SQL.</strong><br>
  A <code>$__search</code> macro for TiDB Cloud Lake and any other Databend-backed warehouse.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="Apache-2.0" />
  <img src="https://img.shields.io/badge/go-1.19%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.19+" />
  <img src="https://img.shields.io/badge/dependencies-none-4f9d69" alt="No dependencies" />
  <img src="https://img.shields.io/badge/conformance-47%2F47%20live-4f9d69" alt="47 of 47 conformance cases passing against a live warehouse" />
</p>

<p align="center">
  <a href="#why">Why</a> • <a href="#quick-start">Quick start</a> • <a href="#syntax">Syntax</a> •
  <a href="#the-one-search-function-rule">Engine rules</a> • <a href="#grafana">Grafana</a> •
  <a href="#testing-against-a-real-warehouse">Conformance</a> • <a href="pipeline/">Pipeline</a>
</p>

Databend has a genuinely strong text-search engine — BM25 relevance through `score()`,
order-sensitive phrase search, English stemming, index-backed fuzzy matching, Ngram substring
search. What it has no equivalent of is the query *language* an engineer arriving from
Elasticsearch already knows. lake-search closes that gap by **translating** rather than passing
through, so the habits people already have either work or fail loudly.

- 🔍 **Lucene-style syntax** — `field:value`, `field:(a OR b)`, `"phrases"`, `AND`/`OR`, `-exclude`, `field:[a TO b]`, `field:>100`, `field:*`, `term~2`, `pref*`, `/regex/`, `term^2`
- 🧨 **Kills the silent failures** — a fuzzy `term~N` and a wildcard `*` return **zero rows and no error** inside this engine's search syntax; they are rewritten, not forwarded
- 🧠 **Knows the one-search-function rule** — boolean full-text logic is folded into a single `query()` call, because two search functions per statement is `[1065]`
- 🔁 **Rewrites what the engine gets wrong** — `a OR -b` has its negative clause silently dropped here, so De Morgan folds it into the one clause shape that evaluates correctly, still inside a single `query()`
- 🧮 **Excludes with an anti-join, not a bare `NOT`** — `NOT (query(x))` returns **zero** rows rather than every row when `x` matches nothing, so `-pdctl` used to blank the screen
- 📦 **Zero dependencies** — standard library only, so it vendors into a Grafana datasource plugin without dragging anything along
- 🔌 **Schema-driven** — describe your table once; unknown fields route into a `VARIANT` column, which is what makes open-ended log schemas searchable
- 📊 **A Grafana macro** — `$__search(msg, '$q')` expands in the datasource backend, retiring the hidden predicate-generating dashboard variable
- ✅ **Conformance by row count, not by exit code** — 47 cases asserting *relationships between counts*, because a wrong query here is indistinguishable from an empty result
- 🧪 **Verified live** — every engine claim below was measured on a running warehouse, not read off a doc page

<img alt="Lucene-style search text compiled to a Databend SQL predicate" src="docs/img/pipeline.svg">

## Quick start

```console
$ lake-search compile 'level:(error OR warn) region -snapshot'
((lower(level) = lower('error') OR lower(level) = lower('warn')) AND query('(msg:region) NOT (msg:snapshot)'))
```

One statement, one search function: the field-scoped group becomes ordinary SQL on a plain
column, and the full-text half — including its exclusion — folds into the single `query()` call
the engine allows. On the reference table that predicate returns 955 rows, 296 of them from the
last two hours.

```bash
go install github.com/choudharypankaj/lake-search/cmd/lake-search@latest
```

```
lake-search compile [-score] <query>    print the WHERE predicate
lake-search sql [-table T] <query>      print a complete SELECT
lake-search conform                     print the row-count conformance script
```

Or as a library:

```go
import "github.com/choudharypankaj/lake-search/databend"

r, err := databend.CompileString(userInput, databend.K8sLogs())
// r.SQL       -> predicate, safe to splice into WHERE
// r.Warnings  -> "this will be a full scan", "fuzziness ignored", …
// r.UsesMatch -> whether score() is legal alongside it
```

## Why

The gap is worse than a missing feature, because the familiar syntax does not error — it silently
returns nothing:

| What you type | What Databend does | What you conclude |
| --- | --- | --- |
| `snapshot~1` | `~1` is not understood; **0 rows** | "there are no matches" |
| `snapsh*` | the term is **truncated at the star**, and `snapsh` is not a token; **0 rows** | "prefix search is broken" |
| `snapshot*` | truncated to `snapshot`, which *is* a token; **full result set** | "prefix search works!" |
| `reg*on` | truncated at the star, so this searches for `reg`; **36 rows, none about regions** | "there were 36 region events" |
| `pod:tikv-??????` | `?` is not a wildcard, it is compared literally; **0 rows** | "no pods match" |
| `no`, `not`, `to`, `is` | 33 English stopwords are deleted from the query; **0 rows** | "the word isn't in the logs" |
| `"not ready"` | the phrase loses `not` and stops being a phrase; **9x too many rows** | "there are 2,320 of these" |
| `"peer stat*"` | inside quotes the star is punctuation: the phrase splits there and `stat` is not a token; **0 rows** against 88,441 for `peer status` | "there are no peer status lines" |
| `msg:not` | the value is read as an operator and the filter disappears; **every row** | "everything matches" |
| `http://a.com` | the colon reads as a field selector; **0 rows** | "that URL isn't in the logs" |
| `-absent_term` | the search function prunes the scan; **0 rows** | "nothing survives the exclusion" |
| *(empty box)* | `match(col,'')` matches nothing; **0 rows** | "there are no logs" |

Every row above is rewritten into SQL that answers the question that was asked, with one
exception, and the exception is marked: `"peer stat*"` is **explained rather than rewritten**.
Quotes and a wildcard ask for contradictory things — one says "these exact characters in this
order", the other says "any token shaped like this" — and guessing which one was meant would be
inventing a query nobody typed. The compiler says what the engine will do with it instead:
measured on two disjoint windows, `"peer stat*"` returns 0 both times where `peer status` returns
88,441 and 38,076.

The exclusion row is the most quietly wrong, and it is the one a responder is most likely to hit:
excluding a noise pattern that has stopped being emitted in the selected window empties the screen
rather than leaving it untouched. It is covered under
[engine rules](#why-a-leading-not-is-compiled-as-an-anti-join).

The last row is the sharpest, and its reason is narrower than it looks — it is the **empty
argument** that poisons the statement, not the boolean around it. An empty search function prunes
the index scan before any surrounding boolean is evaluated, so even `(1=1 OR match(msg,''))`
returns zero. A *non-empty* search function composes with ordinary SQL exactly: measured,
`query('msg:peer')` is 109,950, `lower(level) = lower('warn')` is 9,287, their conjunction 341,
and their disjunction 118,896 — the union to the row.

## Syntax

| Lucene | Databend |
| --- | --- |
| `term` | `query('col:term')` |
| `"two words"` | `query('col:"two words"')` — order-sensitive |
| `a b`, `a AND b` | `query('(col:a) AND (col:b)')` — one call, not two |
| `a -b` | `query('(col:a) NOT (col:b)')` — bare and trailing, never `AND NOT` |
| `-a -b` | De Morgan to `(col:a) OR (col:b)`, then excluded by anti-join — **not** a bare `NOT` |
| `a OR -b` | De Morgan to `(b) NOT (a)`, then excluded the same way |
| `term~2` | `match(col, 'term', 'fuzziness=2')` |
| `pref*`, `*sub*`, `a*b`, `a?b` on the text column | one **token**: `lower(col) RLIKE '(^\|[^a-z0-9])pref[a-z0-9]*([^a-z0-9]\|$)'`, `*` being any run of token characters and `?` exactly one. A star does not cross a word boundary — served as `LIKE '%reg%on%'`, `reg*on` is exactly `RLIKE 'reg.*on'` and 4,196 of its rows contain no word matching reg…on at all. It does not stem either, so the bare token still finds inflections the pattern does not |
| a wildcard the tokenizer would split (`tikv-tikv-*`, `*0.0.0.0:8686/x*`) | `LIKE` — it cannot describe one token, so the substring reading is kept and the warning says which one was used |
| `pod:tikv*`, `pod:a?b` on a plain column | `LIKE 'tikv%'`, `LIKE 'a_b'` — anchored, because on a value column a prefix really does mean "the value starts with" |
| a stopword (`no`, `not`, `to`, …) | `lower(col) RLIKE '(^\|[^a-z0-9])no([^a-z0-9]\|$)'` — the index deletes these 33 words, so they are matched by scan |
| `"a b"` the analyzer cuts to one token | `query('col:b')` **and** `lower(col) LIKE lower('%a b%')` — the token keeps the index and its stemming, the scan checks the adjacency the quotes asked for. A phrase the analyzer leaves alone, `"snapshots"` included, is untouched: it is 17,595 rows through the index against 9 as a substring, because the index stems |
| `"a b"` the analyzer empties | `lower(col) LIKE lower('%a b%')` — nothing left for the index to match |
| `and`, `or`, `not` — any case | an operator only where an operator is **grammatical**: `and`, `or`, `&&` and `\|\|` between two terms, `NOT` before a term. Anywhere else — the whole query, a field's value, the only thing inside a `field:(…)` group, leading with nothing to its left — it is the word the user typed, matched by a word-boundary scan; read as an operator there, `msg:(not)` and a bare `not` compiled to a filter that matched everything. `NOT` is the one that must be capitalised, because it **inverts** the term it takes while `and`/`or` only join terms that keep their own meaning either way: `msg:(not ready)` is the two words, `msg:(NOT ready)` is the complement of `ready` — 3,537 rows against 711,157 over `ts < '2026-08-19 08:00:00'` (715,185 rows). A *trailing* `and`/`or` is dropped rather than demoted — `region or` is someone mid-keystroke, so it still returns `region`. The cost of the word reading, stated rather than hidden: on a single-valued column `level:(error not warn)` is three ANDed equalities and can never match — write `level:(error -warn)` or `level:(error NOT warn)` |
| `term^2` | `(col:term)^2` inside the one `query()` — reorders `score()`, matches the same rows |
| `/re/`, `field:/re/` | `col RLIKE 're'` — not a search function, but no index serves it |
| `field:value` | `lower(field) = lower('value')` — there is no case-insensitive `=` here |
| `field:(a OR b)` | the group compiles under the field: SQL on a plain column, one `query()` on the text one |
| `-field:value` | `COALESCE(NOT (…), TRUE)` — so `x` and `-x` partition the table |
| `field:>100` | `field > 100` |
| `ts:>2026-08-18 22:30:00` | one instant, space and all; a bound that is not a complete instant is a compile error, because the engine rounds it up to the top of the unit in silence |
| `field:[a TO b]`, `{a TO b}`, `[a TO *]` | `field BETWEEN a AND b`, `>`/`<` — plain SQL, never inside `query()` |
| `field:*` | `field IS NOT NULL AND field <> ''` on a real column; plain `kv['field'] IS NOT NULL` on a bag key, where a key present with an empty value still exists |
| `"key with space":value` | a quoted name is a field name, so a bag key containing a space is reachable |
| `+term` | consumed — adjacency already means AND, and the literal `+term` matches nothing |
| `"a b"~N` | `query('col:"a b"~N')` — real proximity, `N` honoured |
| `http://a.com`, `localhost:3000` | whole-term searches: a colon before `//`, or a port, is not a field selector |
| *(empty)* | `1=1` — and no search function anywhere in the output |
| unknown field | `kv['field']::VARCHAR` via the VARIANT column; `kv.a.b` chains subscripts |

## The one-search-function rule

Verified on a live warehouse (Databend v0.34.0), and it shapes the whole design:
**a statement may contain at most one search function per table.**

```sql
match(msg,'a') AND match(msg,'b')   -- [1065] duplicate search function for table 0
match(msg,'a') AND query('msg:b')   -- same
query('msg:a') AND query('msg:b')   -- same
```

So boolean full-text logic cannot be built with SQL `AND`/`OR`. It has to go *inside* one `query()`
call, and that mini-language has three undocumented behaviours that all fail silently:

| Form | Result |
| --- | --- |
| `(a) AND NOT (b)` | **0 rows** — `AND NOT` is broken in every spelling |
| `(a) NOT (b) AND (c)` | everything after the first `NOT` is **ignored** |
| `(a) OR NOT (b)` | the negative clause is **silently dropped** |
| `NOT (…)` alone | `[1903] Invalid query: Only excluding terms given` |
| `(a) AND (b) NOT (c)` | correct — matches the equivalent LIKE exactly |
| `(a) NOT (b) NOT (c)` | correct — matches the equivalent LIKE exactly |
| `(a) NOT ((b) NOT (c))` | correct — a `NOT` nested in a negative group evaluates properly |
| `msg:peer msg:status` | the default operator is **OR**, not AND |

lake-search emits only the forms that work: negatives are bare and trailing, never `AND NOT`, and
operators are always explicit. The two shapes the engine gets wrong are rewritten rather than
refused — an all-negative search and an `OR` with a negative both fold through De Morgan into one
positive `query()` under a SQL `NOT`:

```
   p1 OR p2 OR NOT n1 OR NOT n2
== NOT( n1 AND n2 AND NOT(p1 OR p2) )
-> NOT( query('(n1) AND (n2) NOT ((p1) OR (p2))') )
```

Still one search function, and the result is still a *text* fragment, so it composes further —
which is only sound because a `NOT` nested inside a negative group evaluates correctly here.
Measured: `query('(region) NOT ((peer) NOT ((store)))')` returns 15,634, exactly
20,144 − 4,853 + 343.

### Why a leading `NOT` is compiled as an anti-join

`NOT (query(x))` is **not** the complement of `query(x)`. The search function is pushed into the
index scan whatever the surrounding boolean, so when `x` matches no row the scan is pruned to
nothing and the `NOT` returns **zero rows instead of every row**. Measured over a 152,317-row
window:

| token | `query()` | `NOT (query())` | anti-join |
| --- | ---: | ---: | ---: |
| `zzzznosuchtoken` | 0 | **0** | 152,317 |
| `qqqqwwww` | 0 | **0** | 152,317 |
| `pdctl` | 0 | **0** | 152,317 |
| `tiflash` | 23,381 | 128,936 | 128,936 |

The three absent tokens are the defect. "Everything except a term that does not occur here" is
the whole window, and the bare `NOT` answers nothing — which is exactly what a responder gets
when excluding a noise pattern that has stopped being emitted in the selected time range. No SQL
wrapping rescues it: `COALESCE`, `CASE`, `= FALSE`, `AND TRUE` and `1=1 OR …` were each measured
and each returns zero, because the pruning happens before any of them is evaluated.

So a *leading* negation compiles to an anti-join instead, which keeps tokenised semantics rather
than degrading to a substring `NOT LIKE`:

```sql
COALESCE(msg NOT IN (SELECT msg FROM logs.k8s_logs WHERE msg IS NOT NULL AND query('msg:pdctl')), TRUE)
```

The search function now runs in its own scan, where pruning it to nothing correctly yields an
*empty exclusion set*. It costs a second scan — measured at 0.25–0.32s against 0.12s for the
(wrong) bare `NOT`, on the same half-million rows — and it needs `Schema.Table`, which is the one
thing a `WHERE` fragment cannot infer. A negation that keeps a positive term beside it, `a -b`,
never comes here at all: it stays inside the single `query()` call, where the positive drives the
scan and an absent excluded term costs nothing.

Two consequences worth knowing. The exclusion's search function is in a *different scan*, so it
neither counts against the one-per-table rule — `snapshoot~1 -tiflash` compiles now, and returns
17,608 rows — nor satisfies `score()`, whose binder does not see through the subquery. And
because the outer scan carries no search function, it sees every row including the most recent
hour of ingest, which the inverted index has not caught up on; a fresh row that should have been
excluded is included until the index does.

Structured predicates, `LIKE` and ranges are *not* search functions, so they compose freely with the
single `query()` call. Fuzziness is the exception: it exists only as an option argument to `match()`,
so a fuzzy term spends the statement's one search function and cannot be combined with another
full-text term — which lake-search reports as a compile error rather than emitting SQL that dies
with `[1065]`.

## Testing against a real warehouse

`go test ./...` covers the compiler offline. It cannot tell you whether the *engine* behaves as
documented — and on this engine a wrong query is indistinguishable from an empty result, so a
harness that checks "the SQL executed successfully" proves nothing.

`lake-search conform` generates a script that asserts on **row counts**:

```console
$ lake-search conform > conformance.sql
$ # run conformance.sql through lakesql, the REST endpoint, or a Grafana panel
```

Each statement prints PASS or FAIL. Assertions are relative — `snapshoot~1` must return at least
what `snapshot` returns, `"status peer"` must return nothing when `"peer status"` returns many — so
the suite stays valid as the table grows.

An upper bound is not enough here. `actual <= baseline` passes when `actual` is zero, which is
precisely the failure the suite exists to detect, so cases whose result must be non-empty assert
`narrows` (non-zero **and** not wider), and the negation cases assert `partitions`: `a -b` and `a b`
must add up to `a` exactly, with both halves non-empty. A `NOT` that swallowed its predicate would
return 0 and pass an upper-bound check; it cannot pass this one.

Three cases exist to catch the *engine* rather than the compiler:

- **negation** relies on the optimiser handling an inverted-index scan under `NOT`, which is not
  something `match()` pushdown obviously respects;
- **literal `%`** depends on Databend honouring backslash escapes inside string literals;
- **the time-of-day bound** depends on the engine *not* quietly accepting a truncated timestamp
  literal, which it does — `'2026-08-18T22'` is read as 22:00:00 with no diagnostic, so the
  assertion is a strict inequality against a bound an hour earlier rather than an upper bound.

All 47 cases were run against a live warehouse (Databend v1.2.933-nightly, ~603k rows) and pass.
Every partition identity holds to the row — 19,962 + 5,717 = 25,679 inside one `query()`,
20,309 + 5,370 = 25,679 for the De Morgan fold, 594,705 + 8,277 = 602,982 for a negated bag key,
504,803 + 98,179 = 602,982 for a full-text exclusion, and 305,406 + 297,576 = 602,982 for a
bracket range — so negation is **exact** here, not approximate. (Counts drift between runs only
because the table gains about a thousand rows a minute; each identity is one statement, so each
balances against its own snapshot.)

Three of those are worth reading twice. The bag-key one only balances because negation on a
column is compiled as `COALESCE(NOT (…), TRUE)`: `NOT (col = 'x')` is NULL — and therefore
excluded — wherever the key is absent, which is 97% of rows for most keys. The full-text one only
balances because the exclusion is an anti-join rather than a bare `NOT`. And a partition whose
three counts straddle the search-function boundary cannot be asserted at all, because a search
function sees only the blocks the index scan has reached — which is why the De Morgan case is
scoped under a third term that keeps all three of its counts inside one `query()` call.

Note also that a co-occurrence pair has to be chosen against real data, and chosen for abundance
rather than for mere existence:
`snapshot` and `peer` never appear in the same line in that table, so an intersection case built on
them is vacuous — but a pair meeting in five rows out of half a million is barely better, because it
fails the day those rows age out or the process that wrote them stops running, and the failure looks
exactly like a compiler bug.

## Grafana

[`docs/grafana-macro.md`](docs/grafana-macro.md) has a `$__search(col, '$q')` macro for
[`databendlabs/grafana-databend-datasource`](https://github.com/databendlabs/grafana-databend-datasource).
It is an additive change to the plugin's backend macro registry — one new file and two map entries —
and it retires the hidden predicate-generating dashboard variable entirely.

[`dashboards/`](dashboards/) generates a Grafana dashboard built on the macro: Lucene search box,
component / level / log-format / machine / exclude-machine variables, event deltas, field facets, and
a BM25 relevance panel.

Two Grafana traps worth knowing before you wire this up yourself:

- **Template-variable queries never reach the datasource backend**, so `$__timeFilter` and
  `$__search` do not expand in them. Variable queries have to be plain SQL.
- **A raw-SQL logs panel carries no column hints** — the plugin only attaches them to
  visual-builder queries — so Grafana falls back to "first time field, first string field" to pick
  the log line. With `SELECT ts, level, msg, …` that is `level`, and every line renders as `INFO`.
  Alias to the logs-frame names: `ts AS timestamp, msg AS body, level AS severity, kv AS attributes`.

## score() and the empty search box

`score()` is rejected unless a search function exists **anywhere in the statement**:
`[1065] [SQL-BINDER] Score function must be used together with match or query function`. Because the
`score()` call sits in the select list, **no predicate can rescue it** — `SELECT score() … WHERE 1=0`
still fails. That was verified live, and it rules out the obvious workaround.

`CompileScore` therefore emits a search function that matches nothing:

```sql
SELECT msg, score() FROM logs.k8s_logs WHERE match(msg, 'zzqqnolakesearchmatchqqzz')
```

The binder is satisfied, the panel returns zero rows, and the user sees an empty relevance panel
rather than a red error.

## Notes on proximity

`"a b"~N` is genuine phrase proximity here — `N` is an edit distance, not an on/off switch — and it
is easy to measure otherwise. The full ladder, frozen:

| query | rows | | query | rows |
| --- | ---: | --- | --- | ---: |
| `"region peer"` | 654 | | `"peer status"` | 88,441 |
| `"region peer"~1` | 654 | | `"status peer"` | 0 |
| `"region peer"~2` | 4,593 | | `"status peer"~1` | 0 |
| `"region peer"~3` | 4,593 | | `"status peer"~2` | 88,441 |
| `"region peer"~10` | 4,853 | | | |
| `region peer` (AND) | 4,853 | | | |

Strictly monotone, converging on the unordered AND from below, and the reversed phrase first
matches at exactly `~2` — a transposition costs two, which is textbook Lucene. Sample only the
exact phrase and a large `N` and both land on plateaus, which reads as "`N` is ignored"; an earlier
revision of this library rejected the marker at compile time on precisely that mistake.

## Notes on fuzziness

Edit distance is measured against the **stem**, not the word typed. With the `english_stemmer`
filter, `unreachable` is indexed as `unreach`, so `unreachble` is two edits from the stored token
rather than one and needs `~2`. Any UI exposing fuzziness should say so, or users will see it behave
inconsistently between words the stemmer alters and words it leaves alone.

## Schema

`databend.K8sLogs()` describes a nine-column log table with an inverted index on `msg` and a VARIANT
column `kv` for arbitrary parsed key/value pairs. Define your own for any other table:

```go
s := databend.Schema{
    Default: "body",
    Fields: map[string]databend.Field{
        "body":    {Column: "body", Kind: databend.Text, Ngram: true},
        "service": {Column: "service_name", Kind: databend.String},
        "latency": {Column: "duration_ms", Kind: databend.Number},
    },
    Variant:         "attributes",
    CaseInsensitive: true,
}
```

Fields not in the map are routed to the VARIANT column, which is what makes an open-ended log schema
searchable: the unified TiDB/TiKV/PD/TiCDC log format carries arbitrary `[k=v]` pairs whose names
differ between components, so no fixed column list can cover them.

## Pipeline

[`pipeline/`](pipeline/) holds the collector that fills the table this searches — a Vector DaemonSet
that parses five log formats into the schema above. It lives here because the parser and the schema
have to agree: `kv` is what makes an unknown field searchable, and it only holds anything because the
transform puts it there.

## License

Apache-2.0, matching the Grafana datasource plugin this is intended to be contributed to.
