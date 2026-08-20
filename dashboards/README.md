# Dashboards

`tidb-logs-explorer.py` generates the Grafana dashboard JSON. Run it and POST
the result:

```bash
python3 dashboards/tidb-logs-explorer.py > dash.json
curl -u admin:admin -H 'Content-Type: application/json' -d @dash.json \
  http://<grafana>/api/dashboards/db
```

It is a generator rather than a checked-in blob because the same WHERE clause
appears in nine panels; editing that in raw JSON is how panels drift apart.

**The columns come from the schema, not from this file.** The generator shells out
to `lake-search schema -json` and builds every panel from the resolved roles, so
a panel whose columns the schema does not declare is *refused* rather than
emitted. It used to write `component`, `level`, `node`, `pod`, `source_file` and
`kv['format']` into the SQL directly, which made `--table` a lie: pointing it at
another table produced 129 references to columns that table does not have, valid
JSON, and `[1065]` on every panel with no check of any kind.

```bash
python3 dashboards/tidb-logs-explorer.py --schema my-logs.json > dash.json
python3 dashboards/tidb-logs-explorer.py --preset k8s-logs-line --body line > dash.json
lake-search schema -preset k8s-logs -json > s.json   # if Go is not available here
python3 dashboards/tidb-logs-explorer.py --schema-json s.json > dash.json
```

Refusals go to stderr, one line each, naming what was missing:

```
$ python3 dashboards/tidb-logs-explorer.py --schema ../testdata/schema-app-logs.json
refused: facet filters dropped: the schema has no `component`, `node`
refused: panel `Pods emitting` refused: the schema has no `pod`
refused: facet tables refused: the schema declares no string field to count by
```

That run emits 9 panels instead of 12 and **zero** references to `component`,
`node`, `pod` or `source_file`. Give it the fields that table does have and the
facets come back:

```bash
python3 dashboards/tidb-logs-explorer.py --schema testdata/schema-app-logs.json \
  --facet service --facet route --facet-table service --facet-table route \
  --logformat-key ''
```

The flags: `--schema` / `--preset` / `--schema-json` choose the schema, `--table`
overrides the table it names (same shape, different table — for pointing at a
copy, not at a different deployment), `--body` chooses the log body and searched
column, `--pattern` the field the fingerprint groups on, `--facet` and
`--facet-table` the filter variables and count tables, and `--logformat-key` the
attribute-bag key offered as the log-format filter.

A `--body` that is not a field of the schema, or is not full-text indexed, is
refused outright rather than emitted:

```
$ python3 dashboards/tidb-logs-explorer.py --body line
--body 'line' is not a field of this schema; it has: component, level, message, …
```

`--body line` therefore requires the derived-column migration
([`round4-live-migration.sql`](../../lake-search-handoff/round4-live-migration.sql))
to have run, and the `k8s-logs-line` schema to be the one in use — the default
`k8s-logs` schema does not declare a `line` field, so asking for it fails at
generation time instead of at query time.

The two have to move together. What the panel *shows* and what the index
*searches* must be the same text, or a reader sees a line, searches for a word
in it and is told there are no matches — which is the bug the migration exists to
fix, reintroduced at the display layer. It is also a widening: measured on a
967,912-row frozen copy, `query('msg:snapshot')` is 17,649 and
`query('line:snapshot')` is 25,488, so every saved panel link returns 44% more
rows afterwards. Typing `msg:` still means exactly what it used to.

The pattern panels deliberately stay on `msg` either way — `--pattern` overrides
it. They group lines by shape, and the attribute values the derived column
appends are precisely the varying part, so folding them in would turn one
pattern into hundreds.

## What it demonstrates

Every panel filters through `$__search(<body>, ${search:sqlstring})`, where
`<body>` is whatever `--body` named. There are no
hidden predicate-building variables: v3 of this dashboard carried `pred`,
`predscore` and `scoreexpr`, which leaked generated SQL into every shared URL —

```
...&var-search=snapshot&var-pred=match%28msg,%27snapshot%27%29&var-predscore=...
```

— and pinned a stale predicate into any link someone pasted into a ticket.

Three panels exist to answer questions the search box cannot, rather than to look busy:

| Panel | Why |
| --- | --- |
| **Event deltas** | Each fingerprint's count in this window against the preceding window of equal length. `events_before = 0` means new behaviour — the one thing no built-in Grafana panel type will tell you. |
| **Top pods / Top nodes** | Field facets — the per-value counts a dedicated log UI puts in a sidebar. |
| **Distinct patterns** stat | A jump means genuinely new log shapes, not merely more volume. |

Still not possible through SQL, and honestly out of reach here: **live tail**
(the plugin declares no streaming support, so refresh is polling) and
**match highlighting** (Grafana highlights only when a datasource returns
`searchWords`, which this one does not).

## Datasource settings worth having

The plugin implements more than the dashboard uses. Set these on the datasource
and three further features switch on:

| Setting | Enables |
| --- | --- |
| `logsTable`, `logsTimeColumn`, `logsLevelColumn`, `logsMessageColumn` | log context ("surrounding lines") and the Explore log-volume histogram |
| `defaultAdHocTable` | ad-hoc filters — click a value, filter by it |
| `secureJsonData.password` | keeps the lake password out of `GET /api/datasources`. Put the DSN in `jsonData` **without** a password; the backend overrides it from the secure field. |

Ad-hoc filters rewrite the interpolated SQL, so check the Event deltas panel
after enabling them — it is the one panel built on CTEs.
