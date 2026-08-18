# Ingest pipeline

`vector-logs.yaml` is the collector half of this system: a Vector DaemonSet that
tails every container on the node, shapes each line into the nine-column
`logs.k8s_logs` schema that [`databend.K8sLogs()`](../databend/schema.go)
describes, and writes it to TiDB Cloud Lake through Vector's `databend` sink.

It lives here rather than in a separate repo because the parser and the search
schema have to agree: `kv` is what makes an unknown field like `error=` or
`changefeed=` searchable, and it only holds anything because the transform puts
it there.

```bash
kubectl apply -f pipeline/vector-logs.yaml
kubectl rollout restart daemonset/vector -n logging
```

The sink DSN comes from a secret (`vector-lake`, key `dsn`), not from this file.

## Validate before you roll

VRL failures are config-load failures: a bad transform takes out log shipping on
every node, one node at a time as the DaemonSet rolls. Compile it inside a pod
that already has the binary, and dry-run it against real lines, before applying:

```bash
POD=$(kubectl get pods -n logging -l app=vector -o jsonpath='{.items[0].metadata.name}')

# does it compile?
kubectl cp /tmp/vector.yaml logging/$POD:/tmp/vector.yaml
kubectl exec -n logging $POD -- vector validate --no-environment /tmp/vector.yaml

# what does it actually produce? one {"message": "...", "kubernetes": {...}} per line
kubectl cp /tmp/input.json logging/$POD:/tmp/input.json
kubectl cp /tmp/shape.vrl logging/$POD:/tmp/shape.vrl
kubectl exec -n logging $POD -- vector vrl -i /tmp/input.json -p /tmp/shape.vrl
```

`vector vrl` is the part worth the trouble. Every parser bug found here was
found by reading its output against a line copied from a real pod, not by
reasoning about the regex.

## What the transform does

**`component` comes from Kubernetes labels**, in precedence order — container
name, `app`, `k8s-app`, `app.kubernetes.io/name`, `app.kubernetes.io/component`,
last match winning. It used to come from a pod-name prefix, which put 68.9% of
the `component=tidb` bucket (20,656 rows/hour) into it from `tidb-operator` and
`tidb-controller-manager`, making the facet mean "TiDB, or something that
manages TiDB". Where the role label is generic on its own — `controller` is
prometheus-operator, `csi-driver` is aws-ebs-csi-driver — `kv.service` carries
the chart name.

**The message is parsed by detected format**, and `kv.format` records which
branch matched, so "nothing matched" is a number you can watch instead of a
silent degradation:

| `kv.format` | Shape | Notes |
| --- | --- | --- |
| `tidb` | `[ts] [LEVEL] [file:line] ["msg"] [k=v] …` | bracketed pairs into `kv` |
| `klog` | `I0818 22:47:56.829767 1 proxier.go:1494] "msg" k=v` | severity letter → level, `parse_key_value` for the tail |
| `zap` | ISO-8601 ⇥ LEVEL ⇥ message ⇥ `{json}` | trailing object merged into `kv` |
| `tracing` | `ISO-8601  LEVEL  span{…}: target: message` | Rust `tracing`, no pairs to extract |
| `json` | `{"level":…,"msg":…,"ts":…}` | mapped onto columns, remainder to `kv` |
| `raw` | anything else | whole line in `msg`, `level=UNKNOWN` |

Two rules the parsing follows throughout, both learned the hard way:

- **Quote-aware, never lazy.** A lazy `\[(?P<lmsg>.*?)\]` stops at the first `]`
  *inside* the message, and TiDB messages carry brackets routinely — PD-client
  lines read `"[pd] do http request failed"`, raftstore logs embed
  `"log [committed=0, …]"`. That truncated 3.7% of rows to a stump like `[pd`,
  and since `msg` carries the inverted index the text became **unsearchable**,
  not merely unreadable. Keys need the same treatment: `["msg type"=MsgRequestVote]`.
- **Nothing is dropped silently.** Whatever the `[k=v]` pattern does not consume
  is kept verbatim in `kv.rest_unparsed` — TiKV emits bare `[region 5]` tags
  with no `=`, and losing them quietly is how a log pipeline starts lying about
  what was in the line.

Timestamps come from the line where the line has one, and from the container
runtime's per-line stamp otherwise (klog has no year). `now()` would be the time
Vector read the file, which drifts under backlog.

**`drop_table_art`** discards `tidb-operator`'s multi-line ASCII status tables —
`+------+` and `| id | name | status |`, one log event per border row, 16,595
events/hour at the peak of a reconcile storm. Only lines no parser claimed are
eligible, so a real message that happens to start with `|` survives. Vector
counts the discards as `component_discarded_events_total`, so the volume stays
observable.

## Sink batching

`timeout_secs: 60` is deliberate. The 1s default would write ~86k tiny blocks a
day, and **the lake does not compact itself** — that is scheduled maintenance
you own (`OPTIMIZE TABLE … COMPACT`). Expect up to a minute between a line being
written and it appearing in a query.
