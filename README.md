<h1 align="center">lake-search</h1>

<p align="center">
  <strong>Lucene-style search syntax, compiled to Databend SQL.</strong><br>
  A <code>$__search</code> macro for TiDB Cloud Lake and any other Databend-backed warehouse.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="Apache-2.0" />
  <img src="https://img.shields.io/badge/go-1.19%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.19+" />
  <img src="https://img.shields.io/badge/dependencies-none-4f9d69" alt="No dependencies" />
  <img src="https://img.shields.io/badge/conformance-26%2F26%20live-4f9d69" alt="26 of 26 conformance cases passing against a live warehouse" />
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

- 🔍 **Lucene-style syntax** — `field:value`, `"phrases"`, `AND`/`OR`, `-exclude`, `field:>100`, `field:*`, `term~2`, `pref*`
- 🧨 **Kills the silent failures** — `~N` and `*` return **zero rows and no error** on this engine; they are rewritten, not forwarded
- 🧠 **Knows the one-search-function rule** — boolean full-text logic is folded into a single `query()` call, because two search functions per statement is `[1065]`
- 🚫 **Rejects what would be dropped** — `a OR -b` is a compile error instead of quietly compiling to `a`
- 📦 **Zero dependencies** — standard library only, so it vendors into a Grafana datasource plugin without dragging anything along
- 🔌 **Schema-driven** — describe your table once; unknown fields route into a `VARIANT` column, which is what makes open-ended log schemas searchable
- 📊 **A Grafana macro** — `$__search(msg, '$q')` expands in the datasource backend, retiring the hidden predicate-generating dashboard variable
- ✅ **Conformance by row count, not by exit code** — 26 cases asserting *relationships between counts*, because a wrong query here is indistinguishable from an empty result
- 🧪 **Verified live** — every engine claim below was measured on a running warehouse, not read off a doc page

<img alt="Lucene-style search text compiled to a Databend SQL predicate" src="docs/img/pipeline.svg">

## Quick start

```console
$ lake-search compile 'level:error "peer status" -TiFlash'
(lower(level) = lower('error') AND query('(msg:"peer status") NOT (msg:TiFlash)'))
```

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
| `snapsh*` | `*` is silently ignored; **0 rows** | "prefix search is broken" |
| `snapshot*` | `*` ignored, stem matches; **full result set** | "prefix search works!" |
| *(empty box)* | `match(col,'')` matches nothing; **0 rows** | "there are no logs" |

The last row is the sharpest: `match()` is pushed into the index scan regardless of the surrounding
boolean, so even `('' = '' OR match(msg,''))` returns zero.

## Syntax

| Lucene | Databend |
| --- | --- |
| `term` | `query('col:term')` |
| `"two words"` | `query('col:"two words"')` — order-sensitive |
| `a b`, `a AND b` | `query('(col:a) AND (col:b)')` — one call, not two |
| `a -b` | `query('(col:a) NOT (col:b)')` — bare and trailing, never `AND NOT` |
| `-a -b` | `NOT (query('(col:a) OR (col:b)'))` — De Morgan |
| `term~2` | `match(col, 'term', 'fuzziness=2')` |
| `pref*`, `*sub*` | `lower(col) LIKE lower('pref%')` |
| `field:value` | `lower(field) = lower('value')` — Databend has no `ILIKE` |
| `field:>100` | `field > 100` |
| `field:*` | `field IS NOT NULL AND field <> ''` |
| *(empty)* | `1=1` — and no search function anywhere in the output |
| unknown field | `kv['field']::VARCHAR` via the VARIANT column |

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
| `(a) AND (b) NOT (c)` | correct — matches the equivalent LIKE exactly |
| `(a) NOT (b) NOT (c)` | correct — matches the equivalent LIKE exactly |
| `msg:peer msg:status` | the default operator is **OR**, not AND |

lake-search emits only the forms that work: negatives are bare and trailing, never `AND NOT`;
operators are always explicit; and `a OR -b` is rejected at compile time rather than quietly
compiled into `a`. An all-negative search is folded through De Morgan into one positive `query()`
under a SQL `NOT`.

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

Two cases exist to catch the *engine* rather than the compiler:

- **negation** relies on the optimiser handling an inverted-index scan under `NOT`, which is not
  something `match()` pushdown obviously respects;
- **literal `%`** depends on Databend honouring backslash escapes inside string literals.

All 26 cases were run against a live warehouse (Databend v1.2.933-nightly, ~422k rows) and pass. The
two partition identities hold to the row — 17,446 + 5 = 17,451 and 21,419 + 74,387 = 95,806 — so
negation under an inverted-index scan is **exact** here, not approximate. Note that a co-occurrence
pair has to be chosen against real data: `snapshot` and `peer` never appear in the same line in that
table, and an intersection case built on them is vacuous.

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
