# Upgrading a cluster

go-faster/fs clusters upgrade **in place, one node at a time**, with no cluster
downtime. This guide covers the mechanism; a full deployment guide (systemd,
Kubernetes, Helm) is separate.

## Schema versioning

The cluster carries a single monotonic **schema version** — the on-disk record
formats (object sidecars, bucket records) and the etcd control-plane layout
taken together. The current version a binary implements is
`etcd.SchemaVersion`. The value the running cluster agreed on is stored in etcd
at `<prefix>/meta/schema-version`.

Two rules make mixed-version operation safe during a rolling upgrade:

- **A node never joins a cluster newer than itself.** On startup a node compares
  its `SchemaVersion` against the recorded one; if the cluster is newer it
  refuses to start rather than misread a format it does not understand. This
  blocks a stale binary (e.g. a botched rollback) from corrupting a migrated
  cluster.
- **A newer binary does not bump the schema just by joining.** It runs at the
  cluster's current schema — still writing the old format — until an explicit
  migration raises the version. So a half-upgraded cluster never has one node
  unilaterally break its peers.

Readers tolerate their own and older record versions, so old and new binaries
interoperate at the same schema version throughout the upgrade.

## Rolling upgrade procedure

For each node, one at a time:

1. **Stop the node gracefully.** Send `SIGTERM` (systemd `stop`, Kubernetes pod
   termination, `docker stop` all do this). The node drains in-flight requests
   and deregisters from etcd, so the topology shrinks promptly instead of
   waiting out the node's lease TTL.
2. **Replace the binary and start the node.** It rejoins, and its scrubber and
   the auto-rebalancer restore full protection for anything written while it was
   down.
3. **Wait for the cluster to reconverge** before moving to the next node — never
   take a second node down while the first is still catching up (for erasure
   coding, a second missing shard can be unrecoverable). `fs cluster rebalance
   --dry-run` reporting zero misplaced objects confirms convergence.

Reads stay available throughout (data is durable on the surviving nodes); writes
to a strict 3-node cluster briefly retry while a node is down and then succeed —
run with client/SDK retries, as production clients do.

## Applying a schema migration

When an upgrade raises `SchemaVersion`, the new format is not activated until you
migrate — **after every node runs the new binary**:

```sh
fs cluster migrate --config config.yaml --dry-run   # show versions + pending migrations
fs cluster migrate --config config.yaml             # apply them
```

At most one migrator runs cluster-wide (etcd election) and the schema version is
recorded after each migration, so an interrupted run resumes cleanly. Until you
migrate, the cluster keeps operating at the old schema; a node still too old to
understand the cluster's schema refuses to start.

Most upgrades add no migration (the schema version is unchanged) and this step
reports "schema is up to date".
