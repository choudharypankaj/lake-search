# Wiring lake-search into the Grafana Databend datasource

Target: [`databendlabs/grafana-databend-datasource`](https://github.com/databendlabs/grafana-databend-datasource)
(Apache-2.0, Go backend + TypeScript frontend). The integration point is the
backend **macro registry**, not the frontend — which is why this is an additive
change of a few dozen lines rather than a fork of the query editor.

## Why a macro

The plugin is built on `github.com/grafana/sqlds/v5`, whose macro table lives in
`pkg/macros/macros.go` as a flat map:

```go
var Macros = sqlutil.Macros{
    "timeFilter": TimeFilter,  "timeInterval": TimeInterval,  "interval_s": IntervalSeconds, ...
}
```

Adding one entry gives every panel, and Explore, a search predicate:

```sql
SELECT ts, level, component, pod, msg
FROM logs.k8s_logs
WHERE $__timeFilter(ts) AND $__search(msg, '$q')
ORDER BY ts DESC
LIMIT 500
```

Grafana interpolates `$q` in the **frontend**, before the query reaches the
backend, so the macro receives the user's literal text and expands server-side.

This removes three things at once:

- the hidden predicate-generating dashboard variable (the `${pred:raw}` hack),
  because an empty box now compiles to `1=1` inside the macro;
- the separate `1=0` variable for the relevance panel, replaced by the pair
  `$__search_score()` in the WHERE clause and `$__search_score_expr()` in the
  select list;
- every silent-failure trap, because `~N` and `*` are rewritten rather than
  passed through to `query()`, and an exclusion is an anti-join rather than a
  bare `NOT` around a search function.

## The patch

Add `github.com/choudharypankaj/lake-search` to `go.mod` (it has no
dependencies of its own), then create `pkg/macros/search.go`:

```go
package macros

import (
	"fmt"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/data/sqlutil"

	"github.com/choudharypankaj/lake-search/databend"
)

// Search expands $__search(column, 'user text') into a Databend predicate.
//
// The column argument names the full-text column; everything else is taken
// from the schema, so one macro serves any table.
func Search(query *sqlutil.Query, args []string) (string, error) {
	col, text, err := searchArgs(args)
	if err != nil {
		return "", err
	}
	r, err := databend.CompileString(text, schemaFor(col))
	if err != nil {
		return "", err
	}
	// The warnings are the point of this library as much as the SQL is, and a
	// macro returns a string — so they have to travel inside it or die here.
	// See "Getting the warnings to the reader" below.
	return "(" + r.SQL + ")" + databend.WarningComment(r.Warnings), nil
}

// SearchScore is the WHERE-clause half of a panel that also selects score().
//
// It returns the same predicate Search does. The score() problem is NOT
// solvable here — see SearchScoreExpr — and the attempt to solve it here is
// what made this macro throw the user's filter away.
func SearchScore(query *sqlutil.Query, args []string) (string, error) {
	col, text, err := searchArgs(args)
	if err != nil {
		return "", err
	}
	r, err := databend.CompileScore(text, schemaFor(col))
	if err != nil {
		return "", err
	}
	return "(" + r.SQL + ")" + databend.WarningComment(r.Warnings), nil
}

// SearchScoreExpr is the select-list half. It expands to score() when the
// compiled predicate contains a search function and to the constant 0 when it
// does not:
//
//	SELECT $__search_score_expr(msg, '$q') AS relevance, ts, msg
//	FROM logs.k8s_logs
//	WHERE $__timeFilter(ts) AND $__search_score(msg, '$q')
//	ORDER BY relevance DESC
//
// Databend rejects score() unless a search function is present somewhere in
// the statement, and since score() sits in the select list, no predicate can
// rescue it — `SELECT score() … WHERE 1=0` still returns [1065]. The macro
// this replaces "fixed" that by overwriting the predicate with a token chosen
// to match nothing, which satisfied the binder and silently discarded the
// user's filter: `component:tikv` is 189,623 rows and the panel showed 0.
func SearchScoreExpr(query *sqlutil.Query, args []string) (string, error) {
	col, text, err := searchArgs(args)
	if err != nil {
		return "", err
	}
	r, err := databend.CompileScoreExpr(text, schemaFor(col))
	if err != nil {
		return "", err
	}
	// This path raises one advisory nobody else does — fuzziness makes score()
	// a constant, so the rows are right and only the ranking is meaningless —
	// and returning r.SQL bare dropped exactly that one. A comment is legal in
	// the select list too: measured live, `SELECT score() /* … */ AS relevance
	// … WHERE query('msg:snapshot')` returns the same three rows at 4.761965,
	// and `SELECT 0 /* … */ AS relevance` returns the same rows as the bare 0.
	return r.SQL + databend.WarningComment(r.Warnings), nil
}

func searchArgs(args []string) (col, text string, err error) {
	if len(args) == 0 {
		return "", "", fmt.Errorf("%w: $__search needs a column", sqlutil.ErrorBadArgumentCount)
	}
	col = strings.TrimSpace(args[0])

	// sqlutil splits macro arguments on commas, so a search containing a comma
	// arrives as several arguments. Rejoining is lossless — the split is the
	// only transformation applied — and without it `error, retrying` would be
	// truncated at the comma with no diagnostic.
	if len(args) > 1 {
		text = unquote(strings.TrimSpace(strings.Join(args[1:], ",")))
	}
	return col, text, nil
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// schemaFor builds a schema around the named full-text column.
//
// The datasource already stores a logs schema (logsTable, logsTimeColumn,
// logsLevelColumn, logsMessageColumn) — a fuller integration reads the field
// map from there instead of hardcoding it, and reads Table from logsTable.
//
// Table matters for one construct: excluding a full-text term with no positive
// term beside it compiles to an anti-join, which has to name the table.
// Leaving it wrong points that subquery at the wrong table; leaving it empty
// makes that one shape a compile error instead.
func schemaFor(col string) databend.Schema {
	s := databend.K8sLogs()
	if col != "" && col != s.Default {
		s.Default = col
		s.Fields[col] = databend.Field{Column: col, Kind: databend.Text}
	}
	return s
}
```

and register both in `macros.go`:

```go
 var Macros = sqlutil.Macros{
 	...
 	"interval_s":         IntervalSeconds,
+	"search":             Search,
+	"search_score":       SearchScore,
+	"search_score_expr":  SearchScoreExpr,
 	// Legacy aliases
 	"timeFrom": FromTimeFilter,
 	"timeTo":   ToTimeFilter,
 }
```

## Getting the warnings to the reader

This library is careful to say the right thing — *field "to" is not a column;
reading it from the kv VARIANT*, *wildcard `reg*on` is matched as ONE token,
with the word-boundary regex …*, *"no" is a stopword of the index* — and a
`sqlutil.Macros` entry returns
`(string, error)`. There is no advisory channel in that signature, so every one
of those sentences used to end its life inside `r.Warnings` and the reader got
an unexplained empty panel.

The worked example is someone pasting a log line into the search box:

```
Setting logging verbosity level to: info (4)
```

The interior `to:` is a legal field name followed by a colon, so it is read as
a field, routed into the VARIANT where no such key exists, and the conjunction
is false. **0 rows**, against 36 for the same text quoted. The compiler raises
exactly the right warning and nobody sees it.

Two channels, and they are complementary.

**1. The comment, which this library can fill on its own.** All three macros —
`Search`, `SearchScore` and `SearchScoreExpr` — append
`databend.WarningComment(r.Warnings)` to what they return, so the advisories
travel inside the SQL and land in Grafana's query inspector under *Generated
SQL*. It is inert — measured over `ts < '2026-08-19 00:00:00'` (449,893 rows),
`… AND ((lower(msg) LIKE lower('%/*%')) /* lake-search: … */)` returns the same
**101** rows as the predicate alone, and **12 = 12** over the disjoint
[2026-08-19 00:00, 05:00) window (213,315 rows).

Both `*/` and `/*` inside a warning are neutralised, in that order, so a
comment can neither terminate itself early nor open a nested one. Warning text
is not this library's prose — it quotes the user's search value verbatim, and
`msg:"*/"` and `msg:"/*"` are both ordinary things to paste into a search box.
Measured on this engine a nested `/*` is harmless (the same query with a live
`/*` in the comment body still returns 101), because Databend does not nest
block comments; the neutralisation is there because the comment is handed to
whatever the reader pastes it into next, and PostgreSQL — among others — does
nest, where the inner `/*` swallows the closing `*/` and the rest of the
statement disappears into the comment.

**2. Frame notices, which only the plugin can attach.** A notice renders as an
icon on the panel header with the text on hover, which is where someone
staring at an empty panel will actually look. The datasource does not need to
re-compile anything or share state with the macro: the comment from channel 1
is already sitting in `query.RawSQL` by the time the response is built, so the
warnings can be read straight back out of it.

```go
// searchNotices recovers the advisories the $__search macro left in the SQL.
//
// The macro cannot attach them itself — a sqlutil macro returns a string — so
// it writes them into a comment and this reads them back where a data.Frame
// exists to hang them on.
func searchNotices(rawSQL string) []data.Notice {
	const open, close = "/* lake-search: ", " */"
	i := strings.Index(rawSQL, open)
	if i < 0 {
		return nil
	}
	rest := rawSQL[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return nil
	}
	var out []data.Notice
	for _, w := range strings.Split(rest[:j], " | ") {
		out = append(out, data.Notice{Severity: data.NoticeSeverityWarning, Text: w})
	}
	return out
}
```

Attach them wherever your plugin turns a result set into frames — for a
`sqlds`-based datasource that is after `Driver.Query` returns, in the
`QueryData` the plugin registers:

```go
for _, f := range resp.Frames {
	if f.Meta == nil {
		f.Meta = &data.FrameMeta{}
	}
	f.Meta.Notices = append(f.Meta.Notices, searchNotices(query.RawSQL)...)
}
```

Until that lands, every warning added anywhere in this library is invisible in
Grafana, which is why it is worth doing before rather than after the rewrites
that raise them.

## Deploying it — what actually works

**Copying the binary into the pod does not survive a restart.** If the
Deployment sets `GF_INSTALL_PLUGINS`, Grafana **wipes and re-extracts the whole
plugin directory on every start**, even though the plugin is already installed
and the version is unchanged. Verified the hard way: a patched binary and a
backup file next to it were both gone after one pod restart, and the original
31,223,992-byte binary was back.

So the install URL has to point at your build. Only the backend changes, so the
frontend and the other platform binaries come straight from upstream:

```bash
# 1. build just the backend, in a container (the plugin needs Go 1.25)
docker run --rm -v "$PWD":/src -w /src \
  -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 -e GOFLAGS=-mod=mod \
  golang:1.25-alpine go build -ldflags="-s -w" -o gpx_databend_linux_amd64 ./pkg

# 2. take the upstream release and swap in that one file
gh release download v1.4.9 --repo databendlabs/grafana-databend-datasource --pattern '*.zip'
unzip -q databendlabs-databend-datasource-1.4.9.zip
cp -f gpx_databend_linux_amd64 databendlabs-databend-datasource/
zip -qr databend-datasource-lake-search.zip databendlabs-databend-datasource

# 3. publish it somewhere Grafana can fetch, and point the env var at it
kubectl -n <ns> set env deployment/grafana \
  GF_INSTALL_PLUGINS="<zip url>;databendlabs-databend-datasource"
```

Vendor this module into the plugin with a `replace` directive rather than a
network fetch, so the build pins exactly the code you tested:

```
require github.com/choudharypankaj/lake-search v0.0.0
replace github.com/choudharypankaj/lake-search => ./lakesearch
```

Two further requirements: `GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS` must list
`databendlabs-databend-datasource` (the upstream build is unsigned too), and the
Grafana Deployment needs `strategy.type: Recreate` — with a single RWO PVC a
rolling update crash-loops on `index is locked by another process …
unified-search/bleve`.

## Verified live

Through `POST /api/ds/query` against a deployed Grafana 13.2.0. The table is
live and grows by roughly a thousand rows a minute, so the right-hand column is
a dated snapshot rather than an invariant — what the table asserts is the shape
of the left-hand column. Re-measured 2026-08-19 against 508,072 rows:

| Query | Before | After |
| --- | --- | --- |
| `$__search(msg, '')` | 0 rows | **508,072** |
| `$__search(msg, 'snapshoot~1')` | 0 rows | **17,608** |
| `$__search(msg, 'snapsh*')` | 0 rows | **1,019** — right shape, wrong number; corrected below |
| `$__search(msg, 'component:tidb "peer status" -zzznone')` | n/a | **90,091** |
| `$__search(msg, 'region, peer')` | truncated at the comma | **1,248** |
| `$__search_score(msg, '')` | `[1065]` | **0 rows, no error** |
| `$__search_score(msg, 'unreachable')` | n/a | ranked, top score **10.16** |
| `$__search(msg, 'peer OR -status')` | silently meant `peer` | **folded through De Morgan** |

### Since that build

The rows above were measured through the deployed plugin, whose backend still carries the
earlier build of this library. The shapes below were added after it and are measured the same
way the conformance suite measures — `lake-search compile` piped into the warehouse — against
543,806 rows on 2026-08-19. "Before" is the previous build's own output, compiled and counted
identically, not a recollection.

| Query | Before | After |
| --- | --- | --- |
| `level:(error OR warn) "peer status"` | `1=1`, **543,806** — the field *and* the group were dropped, and parsing stopped there, so the phrase went too | **90,091** |
| `peer OR -status` | rejected at compile time | **531,371** |
| `-pdctl` | `NOT (query('msg:pdctl'))`, **0** — an empty screen, because nothing in the window says `pdctl` | **the whole window** |
| `kv.container:vector` | `kv['kv.container']`, **0** | **7,853** |
| `http://0.0.0.0:8686/playground` | `kv['http']`, **0** | **50**, equal to the matching `LIKE` |
| `store_id:[1000000 TO 2000000]` | `kv['store_id'] = '[1000000' AND query('(msg:TO) AND (msg:"2000000]")')` — silently wrong | **1,101** |
| `ts:[2026-08-18 TO 2026-08-19]` | rejected, and the remediation it suggested did not itself compile | **152,317** |
| `ts:>2026-08-18T22:30:00Z` | **222,922** — the bound truncated to 22:00 with no diagnostic | **164,594** |
| `"peer status"~3` | `query('(msg:"peer status") AND (msg:"~3")')`, **0** — a search for the literal token `~3` | **88,441** |
| `"peer status"~3^2` | **0** — the two markers arrive as one word and neither was read | **88,441**, equal to the unboosted form |

### And since *that* build

The same method again — `lake-search compile` piped into the warehouse — over the frozen window
`ts < '2026-08-19 00:00:00'`, **449,893 rows**, so these reconcile against each other and against
the conformance suite. "Before" is what the tree emitted before this pass, compiled and counted.

| Query | Before | After |
| --- | --- | --- |
| `snapsh*` | `LIKE 'snapsh%'`, **1,019** — anchored at character 0 of the message, so 94% of the answer is missing and every row returned is correct | **17,608**, against 17,595 for the bare token |
| `reg*on` | `query('msg:"reg*on"')`, **36** — the engine truncates at the star, and not one of the 36 contains "region" | **17,082**, against 21,278 for the substring reading |
| `pod:tikv-tikv-??????` | `= 'tikv-tikv-??????'`, **0** — `?` was compared literally | **189,623**, equal to `pod:tikv-tikv-*` |
| `to` | `query('msg:to')`, **0** — one of 33 stopwords the analyzer deletes from the query | **130,002**, equal to the RLIKE oracle |
| `msg:not` | `1=1`, **449,893** — the value was lexed as an operator and the filter vanished | **22,850**, equal to the RLIKE oracle |
| `replica -no` | **1,743** — identical to `replica`, because the negated stopword evaporated | **87**, and 87 + 1,656 = 1,743 |
| `"not ready"` | `query('msg:"not ready"')`, **2,320** — identical to `msg:ready`, the phrase silently dropped | **250**, equal to `msg:/not ready/` |
| `"the leader"` | **9,716** | **4** |
| `"snapshots"` | **17,595** — correct, and the first repair of this row broke it | **17,595**, back on the index |
| `ts:>2026-08-18 22:30:00` | `(ts > '2026-08-18' AND query('msg:"22:30:00"'))`, **0** — split at the space, and the clock spent the statement's one search function | **70,681**, equal to the ISO spelling |
| `ts:>"2026-08-18 22:30:00"` | `1=1`, **449,893** | **70,681** |
| `ts:>2026-08-18T22` | **129,009** — silently the top of the hour | a compile error naming both working spellings |
| `rest:*` | `(… IS NOT NULL AND … <> '')`, **333,266** | **433,901** — a bag key present with an empty value still exists |
| `"msg type":MsgRequestVote` | `query('(msg:"msg type") AND (msg:":MsgRequestVote")')`, **0** | **4,312** whole-table, against 5,560 rows carrying the key |
| relevance panel on `component:tikv` | the predicate was replaced by a match-nothing sentinel, **0** | **189,623**, ranked 0 rather than not shown |

### And since *that* build, again

Six of the rows above were repaired in a way that broke or over-claimed
something else, and the corrections are their own measurements. Same method,
same frozen window `ts < '2026-08-19 00:00:00'` (**449,893 rows**), with a
second disjoint window `[2026-08-19 00:00, 05:00)` (**213,315 rows**) in the
last column because a single window cannot tell a fix from a coincidence.

| Query | The repair that was wrong | Now | Second window |
| --- | --- | --- | --- |
| `"snapshots"` | routed to `LIKE '%snapshots%'`, **9** — the index stems and a scan does not, a 1,955x under-match | **17,595**, equal to the bare token | 14 = 14 |
| `""` | `LIKE '%%'`, **449,893** — the whole window offered as the answer to a search for nothing | **0** | 0 |
| `"not ready"` | `LIKE` alone, **250** — right rows, but the leaf left the index | **250**, now `query('msg:ready')` AND the scan | 1,682 = 1,682 |
| `msg:(not)`, bare `not` | `1=1`, **449,893** against a true 22,850 | **22,850**, equal to the word-boundary oracle | 5,958 = 5,958 |
| `msg:(peer AND not)` | `query('msg:peer')`, **109,950** — the clause dropped, 370x | **297** | 270 |
| `msg:(not ready)` | an anti-join, **447,573** — the complement of what was asked | **1,855** | 1,682 |
| `ts:>2026-08-18 22` | `(ts > '2026-08-18' AND query('msg:22'))`, **3,779** against an intended 129,009 — and the statement's one search function spent on a fragment of a timestamp | a compile error, the same one the `T` spelling always gave | — |
| `reg*on` | `LIKE '%reg%on%'`, **21,278** — exactly `RLIKE 'reg.*on'`, so the star crossed word boundaries: **4,196** of those rows contain no word matching reg…on at all, and the token set is a strict subset of the substring one (0 rows the other way) | **17,082**, one token, `RLIKE '(^\|[^a-z0-9])reg[a-z0-9]*on([^a-z0-9]\|$)'` | 5,306, with 1,628 substring-only |
| `*region` vs `region*` | both `'%region%'`, **20,158 = 20,158** — a suffix wildcard indistinguishable from a prefix one | **16,886** and **20,147** | 5,296 and 6,753 |

The `-pdctl` row is a defect in its own right, not a new feature: it was shipped, and it fires
whenever someone excludes a noise pattern that has stopped being emitted in the selected time
range — which the data-plane scale-down makes routine. `NOT (query(x))` is not the complement of
`query(x)`, because the search function is pushed into the index scan whatever the boolean around
it, so an `x` matching nothing prunes the whole scan. A leading negation now compiles to an
anti-join, which keeps tokenised semantics and needs `Schema.Table` — so `schemaFor` in the patch
above should set it from the datasource's configured logs table rather than leaving the default.

The first row is the one to read twice. A field in front of a group was dropped along with the
group, and because the parser stopped at that point the rest of the query went with it — so a
dashboard filtering `level:(error OR warn) "peer status"` showed every row in the table and no
error anywhere.

## Verifying it

Never assert that the query ran. Assert on row counts:

```bash
lake-search conform > /tmp/conformance.sql   # then run it through your lake client
```

Every statement prints PASS or FAIL. The cases that matter most are the ones
that used to fail silently: empty search, `snapshoot~1`, `snapsh*`, and
negation.

## Upstreaming

This is additive — a new file plus two map entries — and touches no existing
behaviour, which makes it a plausible upstream contribution rather than a
permanent fork. The one design question for upstream is whether the field map
should come from the datasource's existing logs-schema settings instead of a
built-in default, which is the natural follow-up once the macro proves itself.
