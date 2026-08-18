#!/usr/bin/env python3
"""Generate TiDB Logs Explorer v4.

Two things change from v3:

  1. The three hidden predicate-building variables (pred, predscore, scoreexpr)
     are gone. Panels call $__search / $__search_score, which expand in the
     datasource backend. That removes the leaked predicates from shareable URLs
     and makes Lucene syntax work instead of failing silently.

  2. Three panels are added to close the largest remaining HyperDX gaps that
     are actually achievable on this engine: event deltas, and two facet
     tables. Live tail and highlighting remain impossible via SQL.
"""
import json

DS = {"type": "databendlabs-databend-datasource", "uid": "afvhxj6os3j7ka"}
TABLE = "logs.k8s_logs"

# Every panel filters the same way. $__search interpolates with :sqlstring so an
# apostrophe in the search box cannot break the literal.
WHERE = ("$__timeFilter(ts)\n"
         "  AND component IN (${component:sqlstring})\n"
         "  AND level IN (${level:sqlstring})\n"
         "  AND $__search(msg, ${search:sqlstring})")

FINGERPRINT = "regexp_replace(regexp_replace(msg,'[0-9a-f]{8,}','?'),'[0-9]+','?')"


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


panels = []

# ---------------------------------------------------------------- help text
panels.append({
    "id": 10, "type": "text", "gridPos": {"x": 0, "y": 0, "w": 24, "h": 4},
    "options": {"mode": "markdown", "content": (
        "**Search** uses Lucene-style syntax, compiled to Databend SQL by the "
        "`$__search` macro ([lake-search](https://github.com/choudharypankaj/lake-search)).\n\n"
        "`snapshot` · `\"peer status\"` (phrase, order matters) · `component:tikv` · "
        "`level:ERROR` · `-TiFlash` (exclude) · `a OR b` · `(a OR b) c` · "
        "`snapshoot~1` (fuzzy) · `snapsh*` (prefix) · `pod:*` (exists)\n\n"
        "Empty box browses everything. Stemming is on — `truncate` finds *Truncating*. "
        "**Fuzziness is measured against the stem**, so `unreachble` needs `~2`, not `~1`. "
        "`a OR -b` is rejected on purpose: the engine would silently drop the `-b`."
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
panels.append(panel(2, "logs", "Log lines", 0, 15, 16, 16,
                    "SELECT ts, level, msg, component, pod, node, source_file\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\nORDER BY ts DESC\nLIMIT 1000",
                    options={"showTime": True, "wrapLogMessage": True, "sortOrder": "Descending",
                             "enableLogDetails": True, "dedupStrategy": "none"}))

panels.append(panel(3, "table", "Top patterns", 16, 15, 8, 16,
                    f"SELECT component,\n       {FINGERPRINT} AS pattern,\n       count(*) AS events\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\n"
                    "GROUP BY component, pattern\nORDER BY events DESC\nLIMIT 30",
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
      AND component IN (${{component:sqlstring}})
      AND level IN (${{level:sqlstring}})
      AND $__search(msg, ${{search:sqlstring}})
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
                         "the one HyperDX analysis feature with no Grafana equivalent, so it is "
                         "built here in SQL.",
                    fieldConfig={"defaults": {}, "overrides": [
                        {"matcher": {"id": "byName", "options": "delta"},
                         "properties": [{"id": "custom.cellOptions",
                                         "value": {"type": "color-text"}},
                                        {"id": "color", "value": {"mode": "continuous-RdYlGr"}}]}]}))

# ------------------------------------------------------------- facets
panels.append(panel(31, "table", "Top pods", 12, 31, 6, 11,
                    "SELECT pod, count(*) AS events\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\n"
                    "GROUP BY pod ORDER BY events DESC LIMIT 20",
                    desc="Field facet, the equivalent of HyperDX's sidebar counts."))

panels.append(panel(32, "table", "Top nodes", 18, 31, 6, 11,
                    "SELECT node, count(*) AS events\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\n"
                    "GROUP BY node ORDER BY events DESC LIMIT 20"))

# ------------------------------------------------------------- relevance
panels.append(panel(4, "table", "Best matches — BM25 relevance", 0, 42, 24, 10,
                    "SELECT score() AS relevance, ts, component, level, msg, pod\n"
                    f"FROM {TABLE}\n"
                    "WHERE $__timeFilter(ts)\n"
                    "  AND component IN (${component:sqlstring})\n"
                    "  AND level IN (${level:sqlstring})\n"
                    "  AND $__search_score(msg, ${search:sqlstring})\n"
                    "ORDER BY relevance DESC\nLIMIT 50",
                    desc="Ranked by BM25 — something ClickHouse's own docs say it does not do. "
                         "Empty search returns no rows by design: score() requires a search "
                         "function anywhere in the statement, so the macro emits one that "
                         "matches nothing rather than letting the panel error."))

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
         "description": "Lucene-style: field:value, \"phrase\", -exclude, OR, term~1, pref*",
         "query": "", "current": {"text": "", "value": ""}, "options": []},
        {"type": "query", "name": "component", "label": "Component", "datasource": DS,
         "query": f"SELECT DISTINCT component FROM {TABLE} ORDER BY component",
         "multi": True, "includeAll": True, "refresh": 2, "sort": 1,
         "current": {"text": ["All"], "value": ["$__all"]}, "options": []},
        {"type": "query", "name": "level", "label": "Level", "datasource": DS,
         "query": f"SELECT DISTINCT level FROM {TABLE} ORDER BY level",
         "multi": True, "includeAll": True, "refresh": 2, "sort": 1,
         "current": {"text": ["All"], "value": ["$__all"]}, "options": []},
    ]},
    "panels": panels,
}

print(json.dumps({"dashboard": dash, "overwrite": True,
                  "message": "v4: $__search macro, event deltas, facets, stat tiles"}))
