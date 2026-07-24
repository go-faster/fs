# Failure model

What a go-faster/fs cluster tolerates, and the precise condition under which
each scheme loses data. This is the contract the chaos suite
(`chaos/chaos_test.go`) enforces in CI.

## Failure domains

Placement spreads an object's fragments across distinct **failure domains**,
cheapest-diverse first: rack, then node, then disk. A domain is a
`cluster.rack` label; a node with no rack is its own domain. Two fragments in
the same domain share fate, so a scheme's guarantees hold only when the cluster
has at least as many independent domains as the scheme has fragments. Label
racks/availability-zones honestly — an `ec:4,2` object needs six independent
domains to actually tolerate the two failures it promises.

## Per-scheme durability

| Scheme | Fragments | Overhead | Tolerates | Data is lost when |
|---|---|---|---|---|
| **rf2.5** (default) | 2 full replicas + 1 half-parity | 2.5× | any 1 domain lost; bit-rot on either replica | **both full-replica domains are lost** before repair rebuilds one (the half-parity alone cannot reconstruct the object) |
| **rf3** | 3 full replicas | 3.0× | any 2 domains lost, once the async third replica has landed | **more than 2 domains lost**, or a second loss **before the third replica lands** (writes ack at W=2; the third is produced asynchronously) |
| **ec:k,m** (default `ec:4,2`, RS(4,2), 1.5×) | k data + m parity shards | (k+m)/k | any **m** shards/domains lost (reads reconstruct from any k) | **more than m** shards/domains are lost |

Key asymmetries:

- **rf2.5** trades storage for a weaker two-domain guarantee: it survives *one*
  full loss, and repairs bit-rot, but the third fragment is parity, not a full
  replica, so losing both full replicas is fatal. Use it as the cheap default
  for single-fault tolerance; use **rf3** or **ec** where two-fault tolerance
  matters.
- **rf3 and ec:k,m both survive two failures**, but ec does so at 1.5× instead
  of 3× — at the cost of needing k+m domains and paying reconstruction CPU on
  reads/repair.

## Acknowledged writes

A write is acknowledged only after its **synchronous quorum** is durable: the
first two full replicas for rf2.5/rf3, or **all** k+m shards for ec. So an acked
object always survives the loss its scheme promises. Sub-quorum writes are
refused, never silently under-replicated. The remainder (rf2.5 parity, rf3 third
replica) is produced behind a bounded async queue and completed by repair if the
node producing it dies — the object is already durable at quorum meanwhile.

## What repair restores, and when it can't

Every node periodically scrubs its disks and feeds each object through the
scheme-aware repairer: it rebuilds a missing/corrupt fragment from the surviving
ones (replica copy, rf2.5 parity recompute, or RS-decode from any k shards),
completes missed async remainders, and verifies stored checksums (catching
bit-rot). Repair restores full protection **as long as the scheme's recovery
threshold still holds** — losses beyond the table above are unrecoverable, which
is why rolling operations take one domain down at a time and wait for
reconvergence (see below).

## Operational implications

- **Single-node loss is always safe** for every scheme (that is the point). The
  survivors keep serving reads (transparent failover) and accept writes for any
  key whose placement can still reach W=2; repair re-establishes full protection
  when the node returns or is replaced.
- **Rolling upgrades / restarts take one domain down at a time** and wait for the
  cluster to reconverge (`fs cluster rebalance --dry-run` reporting zero
  misplaced) before the next — critical for ec, where a second missing shard
  before repair is unrecoverable. See [UPGRADE.md](UPGRADE.md).
- **etcd is a coordination dependency.** An etcd outage stops membership changes,
  rebalancing, and new-node joins, but existing nodes keep serving objects from
  their last-known topology; the cluster resumes coordinating when etcd returns.
- **The control plane fails safe on version skew.** A node whose binary is older
  than the cluster's recorded schema version refuses to start rather than
  misread a migrated format — see [UPGRADE.md](UPGRADE.md).
- **Correlated failure is the real risk.** The scheme guarantees are per
  independent domain; if racks/AZs aren't labeled, "distinct nodes" can still
  share power or a switch. Size domains and schemes for the correlated failures
  you actually need to survive — see [SIZING.md](SIZING.md).

## What is verified

The chaos suite runs a real multi-node cluster under continuous mixed-scheme
load and asserts, across node SIGKILL/restart, disk wipe, node add, and etcd
restart: **no acknowledged write is lost** outside each scheme's documented loss
case above, and **no object stays under-protected after convergence**. The
rolling-upgrade drill additionally asserts **zero failed requests** through a
graceful node-by-node upgrade.
