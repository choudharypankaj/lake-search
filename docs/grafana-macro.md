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
- the separate `1=0` variable for the relevance panel, replaced by
  `$__search_score()`;
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
	return "(" + r.SQL + ")", nil
}

// SearchScore is the variant for panels that also select score(), which
// Databend rejects unless a match()/query() is present in the same statement.
func SearchScore(query *sqlutil.Query, args []string) (string, error) {
	col, text, err := searchArgs(args)
	if err != nil {
		return "", err
	}
	r, err := databend.CompileScore(text, schemaFor(col))
	if err != nil {
		return "", err
	}
	return "(" + r.SQL + ")", nil
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
 	"interval_s":      IntervalSeconds,
+	"search":          Search,
+	"search_score":    SearchScore,
 	// Legacy aliases
 	"timeFrom": FromTimeFilter,
 	"timeTo":   ToTimeFilter,
 }
```

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
| `$__search(msg, 'snapsh*')` | 0 rows | **1,019** |
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
