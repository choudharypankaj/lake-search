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
import argparse
import json
import subprocess
import sys

# ---------------------------------------------------------------- the schema
#
# This generator used to write column names into the SQL: `component`, `level`,
# `node`, `pod`, `source_file`, `kv['format']`, eleven panels and six template
# variables' worth. That made `--table` a lie — pointing it at another table
# emitted 129 references to columns that table does not have, valid JSON and
# [1065] on every panel, with no check of any kind. A generator that emits SQL
# against columns it has not confirmed exist is the silent-failure pattern this
# whole project is about, in generator form.
#
# So the columns come from the schema now, and a panel whose columns are not
# there is REFUSED rather than emitted. The schema is read from
# `lake-search schema -json`, which is the resolved form — aliases expanded,
# every field carrying the column expression it compiles to — so there is one
# source of truth rather than a second copy of the column list maintained here
# by hand.
#
# `--body` overrides which field is the log body and the searched column. That
# is the derived-column migration: `--body line` points the panels at the STORED
# column that concatenates the message with the values of the attribute bag (see
# round4-live-migration.sql). It is not cosmetic and it is not free. What the
# panel shows and what the index searches have to be the same text, or a reader
# sees a line, searches for a word in it and is told there are no matches — the
# bug that migration exists to fix, reintroduced at the display layer. It also
# widens every saved search: measured on a 967,912-row frozen copy,
# query('msg:snapshot') is 17,649 and query('line:snapshot') is 25,488, so a
# panel returns 44% more rows afterwards. `msg:` typed explicitly still means
# exactly what it always did.
parser = argparse.ArgumentParser(
    description="Generate the TiDB Logs Explorer dashboard from a lake-search schema.")
parser.add_argument("--schema", help="schema descriptor JSON file (passed to lake-search)")
parser.add_argument("--preset", help="built-in lake-search schema preset")
parser.add_argument("--schema-json",
                    help="pre-resolved schema, the output of `lake-search schema -json`. "
                         "Use this when the Go toolchain is not available here.")
parser.add_argument("--lake-search", default="go run ./cmd/lake-search",
                    help="how to invoke lake-search (default: go run ./cmd/lake-search)")
parser.add_argument("--table", help="override the table the schema names. Same shape, different "
                                    "table — this does NOT change the column set, so it is for "
                                    "pointing at a copy, not at a different deployment.")
parser.add_argument("--body", help="field to show as the log body and search with a bare term "
                                   "(default: the schema's own default field)")
parser.add_argument("--pattern", help="field the fingerprint groups on "
                                      "(default: `msg` if the schema has it, else the body)")
parser.add_argument("--facet", action="append",
                    help="string field to offer as a filter variable; repeatable. "
                         "Replaces the defaults (component, level, node).")
parser.add_argument("--facet-table", action="append", dest="facet_table",
                    help="string field to get a per-value count table; repeatable. "
                         "Replaces the defaults (pod, node).")
parser.add_argument("--logformat-key", dest="logformat_key", default="format",
                    help="attribute-bag key offered as the log-format filter. "
                         "Empty string drops the filter.")
args = parser.parse_args()


def load_schema():
    """Read the resolved schema, or die saying how to produce one."""
    if args.schema_json:
        with open(args.schema_json) as fh:
            return json.load(fh)
    cmd = args.lake_search.split() + ["schema", "-json"]
    if args.schema:
        cmd += ["-schema", args.schema]
    elif args.preset:
        cmd += ["-preset", args.preset]
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, check=True).stdout
    except FileNotFoundError:
        sys.exit(f"cannot run {cmd[0]!r}: pass --schema-json with the output of "
                 f"`lake-search schema -json`, or --lake-search with a path to the binary")
    except subprocess.CalledProcessError as e:
        sys.exit(f"lake-search schema failed:\n{e.stderr.strip()}")
    return json.loads(out)


SCHEMA = load_schema()
FIELDS = SCHEMA["fields"]
TABLE = args.table or SCHEMA["table"]
if not TABLE:
    sys.exit("the schema names no table and --table was not given")

# `refusals` is printed at the end so the operator sees, in one place, exactly
# which panels this schema did not earn and why.
refusals = []


def col(name):
    """The SQL column expression for a field name, or None if the schema lacks it."""
    f = FIELDS.get(name)
    return f["column"] if f else None


def require(what, *names):
    """True if every name is a field of this schema; otherwise record a refusal."""
    missing = [n for n in names if n not in FIELDS]
    if not missing:
        return True
    refusals.append(f"{what}: the schema has no " + ", ".join(f"`{m}`" for m in missing))
    return False


def role(name):
    """The field name filling a role, or None. Roles are named by field name."""
    r = SCHEMA.get(name) or ""
    return r if r in FIELDS else None


TIME = role("time")
SEVERITY = role("severity")

BODY = args.body or SCHEMA["default"]
if BODY not in FIELDS:
    sys.exit(f"--body {BODY!r} is not a field of this schema; it has: "
             + ", ".join(sorted(FIELDS)))
if FIELDS[BODY]["kind"] != "text":
    sys.exit(f"--body {BODY!r} is kind {FIELDS[BODY]['kind']!r}, not full-text indexed: "
             f"$__search on it would not reach an index")

# The fingerprint groups lines by shape, so it wants the *message*, not the
# derived surface: the attribute values `line` appends are precisely the varying
# part, and folding them in turns one pattern into hundreds. `msg` when the
# schema has it, the body otherwise.
PATTERN = args.pattern or ("msg" if "msg" in FIELDS else BODY)
if PATTERN not in FIELDS:
    sys.exit(f"--pattern {PATTERN!r} is not a field of this schema")

if args.table and not (args.schema or args.preset or args.schema_json):
    print("note: --table changed the table but not the column set, because no schema was given. "
          "Every panel still selects the default schema's columns.", file=sys.stderr)

if TIME is None:
    sys.exit("this schema declares no time field, so no panel here can be built: every one of "
             "them is bounded by $__timeFilter and ordered by time")

TS = col(TIME)
BODY_COL = col(BODY)

DS = {"type": "databendlabs-databend-datasource", "uid": "afvhxj6os3j7ka"}

# ---------------------------------------------------------------- the facets
#
# A facet is a plain string field the reader filters and groups by. Which fields
# those are is a property of the deployment, so the list is data: the defaults
# below are intersected with what the schema actually declares, and `--facet`
# replaces them outright for a table shaped differently. A facet the schema does
# not have is dropped and reported, never emitted.
# The severity role is not named here. It joins the list under whatever name the
# schema gives it, so a table whose level column is called `sev` still gets the
# filter and a table with no severity at all gets no phantom one.
DEFAULT_FACETS = ["component", "node"]
DEFAULT_FACET_TABLES = ["pod", "node"]


def keep(names, what):
    """Drop what this schema does not declare, and the duplicates, and say so.

    Deduplication is by resolved *column*, not by name. Aliases make that
    necessary: in a schema where `level` is an alias of `sev`, asking for the
    severity role and for `level` is asking for the same column twice, and two
    Grafana variables over one column are two filters that silently fight.
    """
    have, seen, gone = [], set(), []
    for n in names:
        if n not in FIELDS:
            gone.append(n)
            continue
        c = col(n)
        if c in seen:
            continue
        seen.add(c)
        have.append(n)
    if gone:
        refusals.append(f"{what} dropped: the schema has no "
                        + ", ".join(f"`{m}`" for m in gone))
    return have


wanted = list(args.facet) if args.facet else list(DEFAULT_FACETS)
if SEVERITY:
    wanted.insert(min(1, len(wanted)), SEVERITY)
FACETS = keep(wanted, "facet filters")
FACET_TABLES = keep(list(args.facet_table) if args.facet_table else list(DEFAULT_FACET_TABLES),
                    "facet tables")

# The primary facet is the one the machine lists chain on and the pattern table
# groups by. It is the first declared facet rather than a hardcoded `component`,
# so a table with different dimensions still gets the chaining behaviour.
PRIMARY = FACETS[0] if FACETS else None

# Only one facet gets an exclude-variable, because "exclude one noisy machine"
# is a real workflow and "exclude one of everything" is UI clutter. `node` when
# the schema has it.
EXCLUDE = "node" if "node" in FACETS else None

# The log-format facet reads a key of the attribute bag rather than a column, so
# it needs a catch-all bag — a prefixed bag is addressed explicitly and is the
# wrong place to go looking for an unprefixed key.
#
# A bag key's existence is not knowable from the schema, which is the point of a
# bag, so this cannot be validated the way a column can. It degrades quietly
# rather than badly: coalesce puts a missing key in the `legacy` bucket, so a
# table without the key gets a filter with one value in it instead of a broken
# panel. `--logformat-key` names a different key; `--logformat-key ''` drops the
# filter outright.
BAGS = SCHEMA.get("bags") or []
CATCH_ALL = next((b["column"] for b in BAGS if not b.get("prefix")), None)
LOGFORMAT = None
if not args.logformat_key:
    pass
elif CATCH_ALL:
    LOGFORMAT = f"coalesce({CATCH_ALL}['{args.logformat_key}']::VARCHAR, 'legacy')"
else:
    refusals.append("log-format filter dropped: the schema declares no catch-all attribute bag, "
                    f"so there is no `{args.logformat_key}` key to read")


# Every panel filters the same way. $__search interpolates with :sqlstring so an
# apostrophe in the search box cannot break the literal.
#
# FILTERS is separate from the time predicate because the Event deltas panel
# needs the same filters over a *different* window; before this it repeated them
# by hand, which is how the two halves of a comparison quietly stop comparing.
#
# The exclude filter's variable always carries the sentinel value "(none)", which
# matches no real value, so the predicate is valid SQL when nothing is being
# excluded — an empty multi-value variable would interpolate to `x NOT IN ()` and
# fail to parse.
def build_filters():
    parts = []
    for name in FACETS:
        parts.append(f"  AND {col(name)} IN (${{{name}:sqlstring}})")
    if EXCLUDE:
        parts.append(f"  AND {col(EXCLUDE)} NOT IN (${{exclude_{EXCLUDE}:sqlstring}})")
    if LOGFORMAT:
        # Rows ingested before the multi-format parser have no format key at
        # all, and `IN (...)` drops NULLs — which would have silently hidden
        # every historical row the moment this filter existed. coalesce puts
        # them in a "legacy" bucket that the variable lists like any other.
        parts.append(f"  AND {LOGFORMAT} IN (${{logformat:sqlstring}})")
    parts.append(f"  AND $__search({BODY_COL}, ${{search:sqlstring}})")
    return "\n".join(parts)


FILTERS = build_filters()
WHERE = f"$__timeFilter({TS})\n" + FILTERS

# The fingerprint masks digits and hex ids so that lines collapse to shapes. It
# reads PATTERN, not the body: see the note where PATTERN is chosen.
PC = col(PATTERN)
FINGERPRINT = f"regexp_replace(regexp_replace({PC},'[0-9a-f]{{8,}}','?'),'[0-9]+','?')"


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
         "ELSE '  \u00b7 last seen ' || to_string(max_ts) END")

panels = []

# ---------------------------------------------------------------- help text
# Examples, not prose. This is read once before typing, so it is the syntax
# strip plus the single caveat that explains UNEXPECTED EXTRA results; the rest
# — fuzziness against the stem, wildcards not crossing spaces, operator words —
# only makes sense once a specific search has already surprised you, and the
# board cannot know which one that was. Those live on the Search field's own
# tooltip, where Grafana shows them while the box is in use, and in
# docs/grafana-macro.md for anyone debugging in earnest.
#
# The examples name real fields of this schema rather than a fixed list, because
# a syntax strip that demonstrates `component:tikv` against a table with no
# component column teaches the reader a query that returns an error.
example_field = PRIMARY or BODY
example_sev = f"`{SEVERITY}:ERROR` \u00b7 " if SEVERITY else ""
panels.append({
    "id": 10, "type": "text", "gridPos": {"x": 0, "y": 0, "w": 24, "h": 2},
    "options": {"mode": "markdown", "content": (
        f"`snapshot` \u00b7 `\"peer status\"` (phrase, order matters) \u00b7 `{example_field}:value` \u00b7 "
        f"{example_sev}`-TiFlash` (exclude) \u00b7 `a OR b` \u00b7 `(a OR b) c` \u00b7 "
        f"`snapshoot~1` (fuzzy) \u00b7 `snapsh*` (wildcard) \u00b7 `{example_field}:*` (exists)\n\n"
        "Empty box browses everything. Stemming is on — `truncate` finds *Truncating*."
    )}
})

# ------------------------------------------------------------- stat tiles
stat_opts = {"options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                         "colorMode": "background_solid", "graphMode": "none",
                         "textMode": "auto", "justifyMode": "auto"}}

panels.append(panel(20, "stat", "Matching events", 0, 2, 6, 4,
                    f"SELECT count(*) AS events\nFROM {TABLE}\nWHERE {WHERE}",
                    desc="Rows matching the search and filters in this time range.",
                    fieldConfig={"defaults": {"color": {"mode": "fixed", "fixedColor": "blue"},
                                              "unit": "short"}, "overrides": []}, **stat_opts))

# Errors, the level histogram and the level facet all need a severity column.
# A table without one is not broken; it just does not offer the feature, and
# saying so beats emitting `count_if(level='ERROR')` against a table that has no
# level and calling the [1065] a user error.
if SEVERITY:
    SEV = col(SEVERITY)
    panels.append(panel(21, "stat", "Errors", 6, 2, 6, 4,
                        f"SELECT count_if({SEV}='ERROR') AS errors\nFROM {TABLE}\nWHERE {WHERE}",
                        fieldConfig={"defaults": {"unit": "short", "color": {"mode": "thresholds"},
                                                  "thresholds": {"mode": "absolute", "steps": [
                                                      {"color": "green", "value": None},
                                                      {"color": "red", "value": 1}]}},
                                     "overrides": []}, **stat_opts))
else:
    refusals.append("panel `Errors` refused: the schema declares no severity role")

panels.append(panel(22, "stat", "Distinct patterns", 12, 2, 6, 4,
                    f"SELECT count(DISTINCT {FINGERPRINT}) AS patterns\nFROM {TABLE}\nWHERE {WHERE}",
                    desc="Log lines collapse to far fewer shapes than they appear to. "
                         "A jump here means genuinely new behaviour, not more of the same.",
                    fieldConfig={"defaults": {"color": {"mode": "fixed", "fixedColor": "purple"},
                                              "unit": "short"}, "overrides": []}, **stat_opts))

if require("panel `Pods emitting` refused", "pod"):
    panels.append(panel(23, "stat", "Pods emitting", 18, 2, 6, 4,
                        f"SELECT count(DISTINCT {col('pod')}) AS pods\nFROM {TABLE}\nWHERE {WHERE}",
                        fieldConfig={"defaults": {"color": {"mode": "fixed", "fixedColor": "green"},
                                                  "unit": "short"}, "overrides": []}, **stat_opts))

# ------------------------------------------------------------- histogram
if SEVERITY:
    SEV = col(SEVERITY)
    panels.append(panel(1, "timeseries", "Events per minute", 0, 6, 24, 7,
                        f"SELECT to_start_of_minute({TS}) AS time,\n"
                        f"       count_if({SEV}='ERROR') AS error,\n"
                        f"       count_if({SEV}='WARN')  AS warn,\n"
                        f"       count_if({SEV}='INFO')  AS info,\n"
                        f"       count_if({SEV} NOT IN ('ERROR','WARN','INFO')) AS other\n"
                        f"FROM {TABLE}\nWHERE {WHERE}\nGROUP BY time\nORDER BY time",
                        fieldConfig={"defaults": {"custom": {"drawStyle": "bars", "fillOpacity": 80,
                                                            "lineWidth": 0,
                                                            "stacking": {"group": "A", "mode": "normal"}},
                                                  "min": 0}, "overrides": []}))
else:
    refusals.append("panel `Events per minute` refused: it breaks the count down by severity and "
                    "the schema declares no severity role")

# ------------------------------------------------------------- logs + patterns
# The Logs panel columns are aliased to Grafana's logs-frame names —
# timestamp / body / severity / attributes. Panels here send raw SQL, and the
# plugin only attaches its ColumnHint.LogMessage / LogLevel / Time markers on
# queries built through the visual query builder, so a raw-SQL frame arrives
# with no indication of which field is the log line. Grafana then falls back to
# "first time field, first string field" — and with the natural column order
# that first string field was the level, so every line rendered as INFO or ERROR
# with the actual message demoted to a detected field. Aliasing is also
# belt-and-braces: `body` is now both the canonical name and the first string
# field, so either rule picks it.
#
# The trailing columns come from the schema's display role, minus the three that
# a logs frame has already spoken for, so a deployment decides what its log
# detail shows instead of this file deciding for it.
DETAIL = [n for n in SCHEMA.get("display", [])
          if n in FIELDS and n not in {TIME, SEVERITY, BODY}]


def sel(name):
    """A display column, aliased to the typed name when it is an expression."""
    c = col(name)
    return c if c == name else f"{c} AS {name}"


logs_cols = [f"{TS} AS timestamp", f"{BODY_COL} AS body"]
if SEVERITY:
    logs_cols.append(f"{col(SEVERITY)} AS severity")
logs_cols += [sel(n) for n in DETAIL]
if CATCH_ALL:
    logs_cols.append(f"{CATCH_ALL} AS attributes")
panels.append(panel(2, "logs", "Log lines", 0, 13, 16, 16,
                    "SELECT " + ", ".join(logs_cols) + "\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\nORDER BY {TS} DESC\nLIMIT 1000",
                    options={"showTime": True, "wrapLogMessage": True, "sortOrder": "Descending",
                             "enableLogDetails": True, "dedupStrategy": "none"}))

pattern_group = f"{col(PRIMARY)},\n       " if PRIMARY else ""
pattern_by = f"{col(PRIMARY)}, pattern" if PRIMARY else "pattern"
panels.append(panel(3, "table", "Top patterns", 16, 13, 8, 16,
                    f"SELECT {pattern_group}{FINGERPRINT} AS pattern,\n       count(*) AS events\n"
                    f"FROM {TABLE}\nWHERE {WHERE}\n"
                    f"GROUP BY {pattern_by}\nORDER BY events DESC\nLIMIT 30",
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
    WHERE {TS} >= to_timestamp(to_unix_timestamp($__fromTime)*2 - to_unix_timestamp($__toTime))
      AND {TS} <  $__fromTime
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

panels.append(panel(30, "table", "Event deltas — this window vs the one before", 0, 29, 12, 11,
                    DELTAS,
                    desc="What changed. Each pattern's count in this window against the "
                         "immediately preceding window of equal length. A pattern with "
                         "events_before = 0 is new; a large positive delta is a spike. "
                         "No built-in panel type computes this, so it is built here in SQL.",
                    fieldConfig={"defaults": {}, "overrides": [
                        wrap("pattern"),
                        {"matcher": {"id": "byName", "options": "delta"},
                         "properties": [{"id": "custom.cellOptions",
                                         "value": {"type": "color-text"}},
                                        {"id": "color", "value": {"mode": "continuous-RdYlGr"}}]}]}))

# ------------------------------------------------------------- facets
# The per-value counts a dedicated log UI puts in a sidebar. One table per facet
# the schema has, laid out left to right; a schema with none gets none.
for i, name in enumerate(FACET_TABLES[:2]):
    panels.append(panel(31 + i, "table", f"Top {name}s", 12 + i * 6, 29, 6, 11,
                        f"SELECT {col(name)}, count(*) AS events\n"
                        f"FROM {TABLE}\nWHERE {WHERE}\n"
                        f"GROUP BY {col(name)} ORDER BY events DESC LIMIT 20",
                        desc=("Field facet — the per-value counts a dedicated log UI puts in a "
                              "sidebar.") if i == 0 else ""))
if not FACET_TABLES:
    refusals.append("facet tables refused: the schema declares no string field to count by")

# ------------------------------------------------------------- relevance
# score() is legal only alongside a search function, so the ranking expression
# is a macro too: $__search_score_expr expands to score() when the search
# compiled to a full-text term and to the constant 0 when it did not.
#
# Selecting score() unconditionally is [1065] for every structured-only search
# — a field filter, a wildcard, an exclusion on its own — and the previous way
# out was worse than the error: $__search_score overwrote the whole predicate
# with a token chosen to match nothing, so the panel came back empty with the
# user's filter discarded. Measured, one field filter was 189,623 rows in the
# frozen window and that panel showed 0 of them.
# Same columns as the log detail, so the two panels show the same row the same
# way — one of them ordered by relevance and the other by time.
rel_cols = [sel(TIME)] + ([sel(SEVERITY)] if SEVERITY else []) + \
           [sel(n) for n in DETAIL] + [sel(BODY)]
rel_filters = "".join(f"  AND {col(n)} IN (${{{n}:sqlstring}})\n"
                      for n in FACETS if n != EXCLUDE)
panels.append(panel(4, "table", "Best matches — BM25 relevance", 0, 40, 24, 10,
                    f"SELECT $__search_score_expr({BODY_COL}, ${{search:sqlstring}}) AS relevance,\n"
                    f"       {', '.join(rel_cols)}\n"
                    f"FROM {TABLE}\n"
                    f"WHERE $__timeFilter({TS})\n"
                    f"{rel_filters}"
                    f"  AND $__search_score({BODY_COL}, ${{search:sqlstring}})\n"
                    f"ORDER BY relevance DESC, {TS} DESC\nLIMIT 50",
                    fieldConfig={"defaults": {}, "overrides": [wrap(BODY)]},
                    desc="Ranked by BM25, straight from the inverted index — whenever there is "
                         "something to rank. A search with no full-text term in it (a field "
                         "filter, a wildcard, an exclusion on its own) has no relevance to "
                         "compute, so the column is a constant 0 and the panel falls back to "
                         "newest-first. The rows are always the same rows the other panels "
                         "show; only the ordering changes."))

# ---------------------------------------------------------------- variables
templating = [
    {"type": "textbox", "name": "search", "label": "Search",
     "description": "Lucene-style. field:value \u00b7 \"phrase\" \u00b7 -exclude \u00b7 a OR b \u00b7 term~1 (fuzzy) "
                    "\u00b7 wild*card (within one word) \u00b7 field:* (exists). Empty browses everything. "
                    "Stemming is on. Open the Search syntax row for the full notes.",
     "query": "", "current": {"text": "", "value": ""}, "options": []},
]


def facet_var(name, label, description="", chain=True):
    """A recency-ranked, staleness-labelled facet variable.

    Ranked by recency and LABELLED rather than filtered to the window, which is
    the one thing that must not happen: Grafana's MultiValueVariable intersects
    the current selection with the freshly-fetched options and, when nothing
    survives, falls back to the FIRST option — not to "All". So dropping dead
    values would mean pinning a value, panning the time range, and silently
    reading different rows. Every value stays selectable; the ones with nothing
    in the window carry their last-seen stamp. sort must be 0, or Grafana
    re-sorts by text and throws the recency order away.
    """
    c = col(name)
    where = ""
    if chain and PRIMARY and name not in {PRIMARY, SEVERITY}:
        # Chained on the primary facet: a machine that runs nothing from the
        # selected component has no business in the list, and an exclude list of
        # 21 nodes to find the 5 that can matter is unusable. Chained on the
        # primary only — not on severity, which would make values vanish from
        # the list as soon as they stopped erroring.
        where = f"WHERE {col(PRIMARY)} IN (${{{PRIMARY}:sqlstring}}) "
    v = {"type": "query", "name": name, "label": label, "datasource": DS,
         # The facet column is an EXPRESSION, not necessarily a name -- a role can
         # be upper(severity_text) -- so it has to be aliased INSIDE the derived
         # table and referred to by that alias outside it. Selecting the raw
         # expression again at the outer level asks the derived table for a
         # column it does not have: `[1065] column severity_text doesn't exist`,
         # and the facet is then dead for every schema whose roles are
         # expressions. The log-format variable below already does it this way.
         "query": (f"SELECT fv AS __value, fv || {STALE} AS __text FROM ("
                   f"SELECT {c} AS fv, max({TS}) AS max_ts FROM {TABLE} {where}GROUP BY {c}) t "
                   f"ORDER BY max_ts DESC"),
         "multi": True, "includeAll": True, "refresh": 2, "sort": 0,
         "current": {"text": ["All"], "value": ["$__all"]}, "options": []}
    if description:
        v["description"] = description
    return v


LABELS = {"component": "Component", "level": "Level", "node": "Machine", "pod": "Pod"}
# Unchained facets first, then the log-format one, then the chained lists, then
# the exclude list. The order is what Grafana shows in the variable bar, and
# reading left to right it goes from the broadest filter to the narrowest.
FACETS.sort(key=lambda n: n not in {PRIMARY, SEVERITY})
def emit_facets(chained):
    # Severity is left unchained: it is low-cardinality and orthogonal, and
    # narrowing it by the primary facet would hide a level that exists.
    for name in FACETS:
        if (name not in {PRIMARY, SEVERITY}) != chained:
            continue
        templating.append(facet_var(
            name, LABELS.get(name, name.replace("_", " ").title()),
            f"Include only these. Narrows with {LABELS.get(PRIMARY, PRIMARY)}." if chained else "",
            chain=chained))


emit_facets(chained=False)

if LOGFORMAT:
    # `format` is a reserved word in Databend, so the expression is aliased.
    chain = (f"WHERE {col(PRIMARY)} IN (${{{PRIMARY}:sqlstring}}) " if PRIMARY else "")
    templating.append(
        {"type": "query", "name": "logformat", "label": "Log format", "datasource": DS,
         "description": "Which parser matched, or `legacy` for rows ingested before the parser.",
         "query": (f"SELECT lf AS __value, lf || {STALE} AS __text FROM ("
                   f"SELECT {LOGFORMAT} AS lf, max({TS}) AS max_ts "
                   f"FROM {TABLE} {chain}GROUP BY lf) t ORDER BY max_ts DESC"),
         "multi": True, "includeAll": True, "refresh": 2, "sort": 0,
         "current": {"text": ["All"], "value": ["$__all"]}, "options": []})

emit_facets(chained=True)

if EXCLUDE:
    # Exclude is its own variable rather than "deselect it from the include
    # list", because excluding one noisy value out of a dozen should be one
    # click, not eleven. includeAll is off so a selection always exists, and the
    # sentinel row is what makes "exclude nothing" expressible.
    EC = col(EXCLUDE)
    chain = (f"WHERE {col(PRIMARY)} IN (${{{PRIMARY}:sqlstring}}) " if PRIMARY else "")
    templating.append(
        {"type": "query", "name": f"exclude_{EXCLUDE}", "label": f"Exclude {EXCLUDE}",
         "datasource": DS,
         "description": f"Drop these. Leave (none) selected to exclude nothing.",
         "query": (f"SELECT __value, __text FROM ("
                   f"SELECT '(none)' AS __value, '(none)' AS __text, 1 AS grp, NULL AS max_ts "
                   f"UNION ALL "
                   f"SELECT {EC}, {EC} || {STALE}, 0 AS grp, max_ts FROM ("
                   f"SELECT {EC}, max({TS}) AS max_ts FROM {TABLE} {chain}GROUP BY {EC}) t"
                   f") u ORDER BY grp DESC, max_ts DESC"),
         "multi": True, "includeAll": False, "refresh": 2, "sort": 0,
         "current": {"text": ["(none)"], "value": ["(none)"]}, "options": []})

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
    "templating": {"list": templating},
    "panels": panels,
}

for r in refusals:
    print("refused:", r, file=sys.stderr)

print(json.dumps({"dashboard": dash, "overwrite": True,
                  "message": f"v5: schema-driven ({len(panels)} panels, body={BODY})"}))
