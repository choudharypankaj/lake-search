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
  passed through to `query()`.

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
// map from there instead of hardcoding it.
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

## Deploying it

The plugin is already running unsigned, so no new signing obstacle:

```bash
mage -v build:linux && npm run build          # produces dist/
kubectl -n <grafana-ns> cp dist/ <pod>:/var/lib/grafana/plugins/databend/
```

`GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS` must already list
`databendlabs-databend-datasource`. **Set `strategy.type: Recreate` on the
Grafana Deployment** before rolling it — with a single RWO PVC a rolling update
crash-loops on `index is locked by another process … unified-search/bleve`.

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
