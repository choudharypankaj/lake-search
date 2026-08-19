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

## What it demonstrates

Every panel filters through `$__search(msg, ${search:sqlstring})`. There are no
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
