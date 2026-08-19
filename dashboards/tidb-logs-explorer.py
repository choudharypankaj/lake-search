#!/usr/bin/env python3
"""Generate TiDB Logs Explorer v4.

Two things change from v3:

  1. The three hidden predicate-building variables (pred, predscore, scoreexpr)
     are gone. Panels call $__search / $__search_score / $__search_score_expr,
     which expand in the datasource backend. That removes the leaked predicates
     from shareable URLs and makes Lucene syntax work instead of failing
     silently.

  2. Three panels are added for the discovery questions the search box cannot
     answer and this engine can: event deltas, and two facet tables. Live tail
     and match highlighting remain out of reach through SQL — the plugin
     declares no streaming, and Grafana highlights only when a datasource
     returns `searchWords`.
"""
import json

DS = {"type": "databendlabs-databend-datasource", "uid": "afvhxj6os3j7ka"}
TABLE = "logs.k8s_logs"

# Every panel filters the same way. $__search interpolates with :sqlstring so an
# apostrophe in the search box cannot break the literal.
#
# FILTERS is separate from the time predicate because the Event deltas panel
# needs the same filters over a *different* window; before this it repeated them
# by hand, which is how the two halves of a comparison quietly stop comparing.
#
# `node NOT IN` is the exclude-machine filter. Its variable always carries the
# sentinel value "(none)", which matches no real node, so the predicate is valid
# SQL when nothing is being excluded — an empty multi-value variable would
# interpolate to `node NOT IN ()` and fail to parse.
FILTERS = ("  AND component IN (${component:sqlstring})\n"
           "  AND level IN (${level:sqlstring})\n"
           "  AND node IN (${node:sqlstring})\n"
           "  AND node NOT IN (${exclude_node:sqlstring})\n"
           # Rows ingested before the multi-format parser have no kv.format at
           # all, and `IN (...)` drops NULLs — which would have silently hidden
           # every historical row the moment this filter existed. coalesce puts
           # them in a "legacy" bucket that the variable lists like any other.
           "  AND coalesce(kv['format']::VARCHAR, 'legacy') IN (${logformat:sqlstring})\n"
           "  AND $__search(msg, ${search:sqlstring})")

WHERE = "$__timeFilter(ts)\n" + FILTERS

FINGERPRINT = "regexp_replace(regexp_replace(msg,'[0-9a-f]{8,}','?'),'[0-9]+','?')"


# Grafana ellipsises a table cell by default, so a 900-char operator line reads
# as truncated even though the whole value is in the response. This is the
# usually solved with a wrap-lines toggle; per-column and always-on is the
# closer fit here, because Grafana persists such a toggle per dashboard rather
# than per user, so one reader's preference becomes everyone's.
def wrap(field):
    return {"matcher": {"id": "byName", "options": field},
            "properties": [{"id": "custom.cellOptions",
                            "value": {"type": "auto", "wrapText": True}}]}


def target(sql):
    return [{"format": "table", "rawSql": sql, "refId": "A"}]


def panel(pid, ptype, title, x, y, w, h, sql, desc="", **kw):
    p = {"id": pid, "type": ptype, "title": title, "datasource": DS,
         "gridPos": {"x": x, "y": y, "w": w, "h": h},
         "targets": target(sql)}
    if desc:
        p["description"] = desc
    p.update(kw)
    return p


# A facet value with nothing in the current window keeps its place in the list and
# says when it was last seen, rather than disappearing — see the Machine comment.
STALE = ("CASE WHEN max_ts >= $__fromTime THEN '' "
         "ELSE '  · last seen ' || to_string(max_ts) END")

panels = []

# ---------------------------------------------------------------- help text
panels.append({
    "id": 10, "type": "text", "gridPos": {"x": 0, "y": 0, "w": 24, "h": 4},
    "options": {"mode": "markdown", "content": (
        "**Search** uses Lucene-style syntax, compiled to Databend SQL by the "
        "`$__search` macro ([lake-search](https://github.com/choudharypankaj/lake-search)).\n\n"
        "`snapshot` · `\"peer status\"` (phrase, order matters) · `component:tikv` · "
        "`level:ERROR` · `-TiFlash` (exclude) · `a OR b` · `(a OR b) c` · "
        "`snapshoot~1` (fuzzy) · `snapsh*` (wildcard) · `pod:*` (exists)\n\n"
        "Empty box browses everything. Stemming is on — `truncate` finds *Truncating*. "
        "**Fuzziness is measured against the stem**, so `unreachble` needs `~2`, not `~1`. "
        "A wildcard matches **within one word**: `snapsh*` finds *snapshot*, never across a space. "
        "Operator words are only operators where one fits — `snapshot or peer` is an OR, "
        "`msg:(not)` searches for the word *not*."
    )}
})

# ------------------------------------------------------------- stat tiles
stat_opts = {"options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                         "colorMode": "background_solid", "graphMode": "none",
                         "textMode": "auto", "justifyMode": "auto"}}

panels.append(panel(20, "stat", "Matching events", 0, 4, 6, 4,
                    f"SELECT count(*) AS events\nFROM {TABLE}\nWHERE {WHERE}",
                    desc="Rows matching the search and filters in this time range.",
                    fieldConfig={"defaults": {"color": {"mode": "fixed", "fixedColor": "blue"},
                                              "unit": "short"}, "overrides": []}, **stat_opts))

panels.append(panel(21, "stat", "Errors", 6, 4, 6, 4,
                    f"SELECT count_if(level='ERROR') AS errors\nFROM {TABLE}\nWHERE {WHERE}",
                    fieldConfig={"defaults": {"unit": "short", "color": {"mode": "thresholds"},
                                              "thresholds": {"mode": "absolute", "steps": [
                                                  {"color": "green", "value": None},
                                                  {"color": "red", "value": 1}]}},
                                 "overrides": []}, **stat_opts))

panels.append(panel(22, "stat", "Distinct patterns", 12, 4, 6, 4,
                    f"SELECT count(DISTINCT {FINGERPRINT}) AS patterns\nFROM {TABLE}\nWHERE {WHERE}",
                    desc="Log lines collapse to far fewer shapes than they appear to. "
                         "A jump here means genuinely new behaviour, not more of the same.",
                    fieldConfig={"defaults": {"color": {"mode": "fixed", "fixedColor": "purple"},
                                              "unit": "short"}, "overrides": []}, **stat_opts))

panels.append(panel(23, "stat", "Pods emitting", 18, 4, 6, 4,
                    f"SELECT count(DISTINCT pod) AS pods\nFROM {TABLE}\nWHERE {WHERE}",
                    fieldConfig={"defaults": {"color": {"mode": "fixed", "fixedColor": "green"},
                                              "unit": "short"}, "overrides": []}, **stat_opts))

# ------------------------------------------------------------- histogram
panels.append(panel(1, "timeseries", "Events per minute", 0, 8, 24, 7,
                    "SELECT to_start_of_minute(ts) AS time,\n"
                    "       count_if(level='ERROR') AS error,\n"
                    "       count_if(level='WARN')  AS warn,\n"
                    "       count_if(level='INFO')  AS info,\n"
                    "       count_if(level NOT IN ('ERROR','WARN','INFO')) AS other\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\nGROUP BY time\nORDER BY time",
                    fieldConfig={"defaults": {"custom": {"drawStyle": "bars", "fillOpacity": 80,
                                                         "lineWidth": 0,
                                                         "stacking": {"group": "A", "mode": "normal"}},
                                              "min": 0}, "overrides": []}))

# ------------------------------------------------------------- logs + patterns
# The Logs panel columns are aliased to Grafana's logs-frame names —
# timestamp / body / severity / attributes. Panels here send raw SQL, and the
# plugin only attaches its ColumnHint.LogMessage / LogLevel / Time markers on
# queries built through the visual query builder, so a raw-SQL frame arrives
# with no indication of which field is the log line. Grafana then falls back to
# "first time field, first string field" — and with the natural column order
# that first string field was `level`, so every line rendered as INFO or ERROR
# with the actual message demoted to a detected field. Aliasing is also
# belt-and-braces: `body` is now both the canonical name and the first string
# field, so either rule picks it.
panels.append(panel(2, "logs", "Log lines", 0, 15, 16, 16,
                    "SELECT ts AS timestamp, msg AS body, level AS severity,\n"
                    "       component, pod, node, source_file, kv AS attributes\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\nORDER BY ts DESC\nLIMIT 1000",
                    options={"showTime": True, "wrapLogMessage": True, "sortOrder": "Descending",
                             "enableLogDetails": True, "dedupStrategy": "none"}))

panels.append(panel(3, "table", "Top patterns", 16, 15, 8, 16,
                    f"SELECT component,\n       {FINGERPRINT} AS pattern,\n       count(*) AS events\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\n"
                    "GROUP BY component, pattern\nORDER BY events DESC\nLIMIT 30",
                    fieldConfig={"defaults": {}, "overrides": [wrap("pattern")]},
                    desc="Fingerprinting: digits and hex ids masked, so 300k lines collapse to a "
                         "handful of shapes. The technique Netflix uses instead of an index; here "
                         "it composes with one."))

# ------------------------------------------------------------- event deltas
DELTAS = f"""WITH cur AS (
  SELECT pattern, count(*) AS c FROM (
    SELECT {FINGERPRINT} AS pattern
    FROM {TABLE}
    WHERE {WHERE}
  ) t GROUP BY pattern
),
prev AS (
  SELECT pattern, count(*) AS c FROM (
    SELECT {FINGERPRINT} AS pattern
    FROM {TABLE}
    WHERE ts >= to_timestamp(to_unix_timestamp($__fromTime)*2 - to_unix_timestamp($__toTime))
      AND ts <  $__fromTime
{FILTERS}
  ) t GROUP BY pattern
)
SELECT coalesce(cur.pattern, prev.pattern) AS pattern,
       coalesce(cur.c, 0)  AS events_now,
       coalesce(prev.c, 0) AS events_before,
       coalesce(cur.c, 0) - coalesce(prev.c, 0) AS delta
FROM cur FULL OUTER JOIN prev ON cur.pattern = prev.pattern
ORDER BY abs(coalesce(cur.c, 0) - coalesce(prev.c, 0)) DESC
LIMIT 25"""

panels.append(panel(30, "table", "Event deltas — this window vs the one before", 0, 31, 12, 11,
                    DELTAS,
                    desc="What changed. Each pattern's count in this window against the "
                         "immediately preceding window of equal length. A pattern with "
                         "events_before = 0 is new; a large positive delta is a spike. This is "
                         "No built-in panel type computes this, so it is built here in SQL.",
                    fieldConfig={"defaults": {}, "overrides": [
                        wrap("pattern"),
                        {"matcher": {"id": "byName", "options": "delta"},
                         "properties": [{"id": "custom.cellOptions",
                                         "value": {"type": "color-text"}},
                                        {"id": "color", "value": {"mode": "continuous-RdYlGr"}}]}]}))

# ------------------------------------------------------------- facets
panels.append(panel(31, "table", "Top pods", 12, 31, 6, 11,
                    "SELECT pod, count(*) AS events\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\n"
                    "GROUP BY pod ORDER BY events DESC LIMIT 20",
                    desc="Field facet — the per-value counts a dedicated log UI puts in a sidebar."))

panels.append(panel(32, "table", "Top nodes", 18, 31, 6, 11,
                    "SELECT node, count(*) AS events\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\n"
                    "GROUP BY node ORDER BY events DESC LIMIT 20"))

# ------------------------------------------------------------- relevance
# score() is legal only alongside a search function, so the ranking expression
# is a macro too: $__search_score_expr expands to score() when the search
# compiled to a full-text term and to the constant 0 when it did not.
#
# Selecting score() unconditionally is [1065] for every structured-only search
# — component:tikv, level:ERROR, snapsh*, pod:* — and the previous way out was
# worse than the error: $__search_score overwrote the whole predicate with a
# token chosen to match nothing, so the panel came back empty with the user's
# filter discarded. Measured, the component:tikv filter is 189,623 rows in the
# frozen window and that panel showed 0 of them.
panels.append(panel(4, "table", "Best matches — BM25 relevance", 0, 42, 24, 10,
                    "SELECT $__search_score_expr(msg, ${search:sqlstring}) AS relevance,\n"
                    "       ts, component, level, msg, pod\n"
                    f"FROM {TABLE}\n"
                    "WHERE $__timeFilter(ts)\n"
                    "  AND component IN (${component:sqlstring})\n"
                    "  AND level IN (${level:sqlstring})\n"
                    "  AND $__search_score(msg, ${search:sqlstring})\n"
                    "ORDER BY relevance DESC, ts DESC\nLIMIT 50",
                    fieldConfig={"defaults": {}, "overrides": [wrap("msg")]},
                    desc="Ranked by BM25, straight from the inverted index — whenever there is "
                         "something to rank. A search with no full-text term in it (a field "
                         "filter, a wildcard, an exclusion on its own) has no relevance to "
                         "compute, so the column is a constant 0 and the panel falls back to "
                         "newest-first. The rows are always the same rows the other panels "
                         "show; only the ordering changes."))

dash = {
    "uid": "tidb-logs-explorer",
    "title": "TiDB Logs Explorer",
    "description": "Log search over TiDB Cloud Lake (Databend FUSE + inverted index), "
                   "with Lucene-style syntax via the $__search macro.",
    "tags": ["logs", "tidb", "lake"],
    "timezone": "utc",
    "schemaVersion": 39,
    "refresh": "30s",
    "time": {"from": "now-30m", "to": "now"},
    "editable": True,
    "templating": {"list": [
        {"type": "textbox", "name": "search", "label": "Search",
         "description": "Lucene-style: field:value, \"phrase\", -exclude, OR, term~1, wild*card",
         "query": "", "current": {"text": "", "value": ""}, "options": []},
        {"type": "query", "name": "component", "label": "Component", "datasource": DS,
         "query": f"SELECT component AS __value, component || {STALE} AS __text "
                  f"FROM (SELECT component, max(ts) AS max_ts FROM {TABLE} GROUP BY component) t "
                  f"ORDER BY max_ts DESC",
         "multi": True, "includeAll": True, "refresh": 2, "sort": 0,
         "current": {"text": ["All"], "value": ["$__all"]}, "options": []},
        {"type": "query", "name": "level", "label": "Level", "datasource": DS,
         "query": f"SELECT level AS __value, level || {STALE} AS __text "
                  f"FROM (SELECT level, max(ts) AS max_ts FROM {TABLE} GROUP BY level) t "
                  f"ORDER BY max_ts DESC",
         "multi": True, "includeAll": True, "refresh": 2, "sort": 0,
         "current": {"text": ["All"], "value": ["$__all"]}, "options": []},
        # Both machine lists are chained on Component: a node that runs no tidb
        # pod has no business appearing in the list while Component=tidb, and an
        # exclude list of 21 nodes to find the 5 that can matter is unusable.
        # Chained on component only — not level, which would make machines
        # vanish from the list as soon as they stopped erroring.
        #
        # Ranked by recency and LABELLED rather than filtered to the window, which
        # is the one thing that must not happen: Grafana's MultiValueVariable
        # intersects the current selection with the freshly-fetched options and,
        # when nothing survives, falls back to the FIRST option — not to "All".
        # So dropping dead values would mean pinning a machine, panning the time
        # range, and silently reading a different machine's logs. Every value
        # stays selectable; the ones with nothing in the window carry their last
        # seen stamp. sort must be 0, or Grafana re-sorts by text and throws the
        # recency order away.
        # `format` is a reserved word in Databend, so the column is aliased.
        {"type": "query", "name": "logformat", "label": "Log format", "datasource": DS,
         "description": "Which parser matched: tidb, klog, zap, tracing, json, "
                        "raw (nothing matched), legacy (ingested before the parser).",
         "query": f"SELECT lf AS __value, lf || {STALE} AS __text FROM ("
                  f"SELECT coalesce(kv['format']::VARCHAR, 'legacy') AS lf, max(ts) AS max_ts "
                  f"FROM {TABLE} WHERE component IN (${{component:sqlstring}}) GROUP BY lf) t "
                  f"ORDER BY max_ts DESC",
         "multi": True, "includeAll": True, "refresh": 2, "sort": 0,
         "current": {"text": ["All"], "value": ["$__all"]}, "options": []},
        {"type": "query", "name": "node", "label": "Machine", "datasource": DS,
         "description": "Include only these nodes. Narrows with Component.",
         "query": f"SELECT node AS __value, node || {STALE} AS __text FROM ("
                  f"SELECT node, max(ts) AS max_ts FROM {TABLE} "
                  f"WHERE component IN (${{component:sqlstring}}) GROUP BY node) t "
                  f"ORDER BY max_ts DESC",
         "multi": True, "includeAll": True, "refresh": 2, "sort": 0,
         "current": {"text": ["All"], "value": ["$__all"]}, "options": []},
        # Exclude is its own variable rather than "deselect it from Machine",
        # because excluding one noisy node out of a dozen should be one click,
        # not eleven. includeAll is off so a selection always exists, and the
        # sentinel row is what makes "exclude nothing" expressible.
        {"type": "query", "name": "exclude_node", "label": "Exclude machine", "datasource": DS,
         "description": "Drop these nodes. Leave (none) selected to exclude nothing. Narrows with Component.",
         "query": f"SELECT __value, __text FROM ("
                  f"SELECT '(none)' AS __value, '(none)' AS __text, 1 AS grp, NULL AS max_ts "
                  f"UNION ALL "
                  f"SELECT node, node || {STALE}, 0 AS grp, max_ts FROM ("
                  f"SELECT node, max(ts) AS max_ts FROM {TABLE} "
                  f"WHERE component IN (${{component:sqlstring}}) GROUP BY node) t"
                  f") u ORDER BY grp DESC, max_ts DESC",
         "multi": True, "includeAll": False, "refresh": 2, "sort": 0,
         "current": {"text": ["(none)"], "value": ["(none)"]}, "options": []},
    ]},
    "panels": panels,
}

print(json.dumps({"dashboard": dash, "overwrite": True,
                  "message": "v4: $__search macro, event deltas, facets, stat tiles"}))
