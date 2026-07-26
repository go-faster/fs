import { useQueryClient } from "@tanstack/react-query";
import {
  getGetMigrationStatusQueryKey,
  useApplyMigrations,
  useGetClusterStatus,
  useGetMigrationStatus,
} from "../api/admin";
import type { ClusterDisk, ClusterNode } from "../api/model";
import { useToast } from "../components/toast";

// Binary byte units, matching the sizing docs (TiB/GiB, not TB/GB).
function fmtBytes(n: number): string {
  if (n <= 0) return "0 B";
  const u = ["B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"];
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), u.length - 1);
  const v = n / 1024 ** i;
  return `${i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)} ${u[i]}`;
}

// Fullness bands mirror the drain watermark (0.9) and the "high" threshold.
type Band = "healthy" | "degraded" | "critical";
function band(f: number): Band {
  return f >= 0.9 ? "critical" : f >= 0.7 ? "degraded" : "healthy";
}

const DRAIN_MARK = 90; // % — the default fullness watermark.

// drainLabel distinguishes a drain in progress from a finished one, and shows
// how much of it is left. Weight only says the disk was taken out of placement;
// has_data says whether its data has actually moved off, which is what makes
// removing it safe, and bytes says how far along the move is.
function drainLabel(d: ClusterDisk): string {
  if (d.has_data === false) return "empty";
  if (d.bytes !== undefined) return `drain ${fmtBytes(d.bytes)}`;

  return "drain";
}

function drainTitle(d: ClusterDisk): string | undefined {
  if (d.data_error) return `Occupancy unknown: ${d.data_error}`;

  if (d.has_data === false) return "Drained: this disk holds no fragments.";

  if (d.has_data) {
    // Absent counters mean the node has not finished anchoring its index, not
    // that the disk is nearly empty — say which it is.
    return d.fragments === undefined
      ? "Draining: this disk still holds fragments (counting them)."
      : `Draining: ${d.fragments.toLocaleString()} fragments left, ${fmtBytes(d.bytes ?? 0)}.`;
  }

  return undefined;
}

function Disk({ d }: { d: ClusterDisk }) {
  const known = (d.total_bytes ?? 0) > 0;
  const drained = d.weight <= 0;
  const f = d.fullness ?? 0;
  const b = band(f);

  const gauge = drained
    ? "gauge gauge--drained"
    : known
      ? `gauge gauge--${b}`
      : "gauge gauge--unknown";

  return (
    <div className="disk">
      <span className="disk__id">{d.id}</span>
      <div
        className={gauge}
        role="meter"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={known ? Math.round(f * 100) : undefined}
        aria-label={`disk ${d.id} ${known ? `${Math.round(f * 100)}% full` : "capacity unknown"}`}
      >
        {known && !drained && (
          <span
            className="gauge__fill"
            style={{ width: `${Math.max(f * 100, 1.5)}%` }}
          />
        )}
        {known && (
          <span className="gauge__mark" style={{ left: `${DRAIN_MARK}%` }} />
        )}
      </div>
      <span className={`disk__pct ${known ? b : "muted"}`}>
        {known ? `${Math.round(f * 100)}%` : "—"}
      </span>
      <span
        className={`disk__w ${drained ? "drained" : ""}`}
        title={drainTitle(d)}
      >
        {drained ? drainLabel(d) : `w${d.weight}`}
      </span>
      <span className="disk__cap">
        {known
          ? `${fmtBytes((d.total_bytes ?? 0) - (d.free_bytes ?? 0))} / ${fmtBytes(d.total_bytes ?? 0)}`
          : "no data"}
      </span>
    </div>
  );
}

// NodeLive renders what only the node itself knows — its queues, runner and
// scrub totals. A node that did not answer says so, with the reason: an empty
// live row would otherwise read as a healthy, idle node.
function NodeLive({ n }: { n: ClusterNode }) {
  if (!n.live) {
    return (
      <span
        className="chip warn"
        title={n.live_error ?? "the node did not report its live state"}
      >
        not reporting
      </span>
    );
  }

  const l = n.live;
  const rebalancing =
    l.rebalance_state === "running" || l.rebalance_state === "waiting";

  return (
    <>
      <span
        className={`chip ${l.repair_queue_depth > 0 ? "on" : ""}`}
        title="Objects with pending async replication/repair work on this node."
      >
        queue {l.repair_queue_depth}
      </span>
      {rebalancing && (
        <span className="chip on">
          {l.rebalance_state} · {l.rebalance_relocated}/{l.rebalance_objects}
        </span>
      )}
      {l.rebuilt_fragments > 0 && (
        <span
          className="chip"
          title="Fragments rebuilt by scrubs on this node."
        >
          rebuilt {l.rebuilt_fragments}
        </span>
      )}
      {l.corrupt_replicas > 0 && (
        <span
          className="chip warn"
          title="Replica payloads that failed checksum verification (bit-rot)."
        >
          corrupt {l.corrupt_replicas}
        </span>
      )}
      {l.objects_held !== undefined && (
        <span
          className={`chip ${(l.never_verified ?? 0) > 0 ? "warn" : ""}`}
          title={coverageTitle(l)}
        >
          {coverageLabel(l)}
        </span>
      )}
      {l.ec_unverified && (
        <span
          className="chip warn"
          title="The node's last scrub pass saw an EC set failing parity verification."
        >
          EC unverified
        </span>
      )}
      {l.version && <span className="node__ver">{l.version}</span>}
    </>
  );
}

// coverageLabel says how stale this node's scrub coverage is. Counts of scrub
// work say how busy the scrubber was; this says whether it is keeping up.
function coverageLabel(l: NonNullable<ClusterNode["live"]>): string {
  const never = l.never_verified ?? 0;
  if (never > 0) return `unverified ${never.toLocaleString()}`;

  if (!l.oldest_verified) return "unverified";

  return `verified ${fmtAge(l.oldest_verified)}`;
}

function coverageTitle(l: NonNullable<ClusterNode["live"]>): string {
  const held = (l.objects_held ?? 0).toLocaleString();
  const never = l.never_verified ?? 0;

  if (never > 0) {
    return `${never.toLocaleString()} of ${held} objects on this node have never been verified.`;
  }

  return `All ${held} objects on this node have been verified since ${l.oldest_verified}.`;
}

// fmtAge renders how long ago something happened, which is what makes a cycle
// falling behind visible: the age recedes.
function fmtAge(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime();
  if (!Number.isFinite(ms) || ms < 0) return "just now";

  const m = Math.floor(ms / 60000);
  if (m < 60) return `${m}m ago`;

  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;

  return `${Math.floor(h / 24)}d ago`;
}

function Rack({ id, nodes }: { id: string; nodes: ClusterNode[] }) {
  let total = 0;
  let free = 0;
  let diskN = 0;

  for (const n of nodes) {
    for (const d of n.disks) {
      diskN++;
      if ((d.total_bytes ?? 0) > 0) {
        total += d.total_bytes ?? 0;
        free += d.free_bytes ?? 0;
      }
    }
  }

  return (
    <section className="rack">
      <header className="rack__head">
        <span className="rack__id">{id}</span>
        <span className="rack__meta">
          {nodes.length} {nodes.length === 1 ? "node" : "nodes"} · {diskN}{" "}
          {diskN === 1 ? "disk" : "disks"}
        </span>
        {total > 0 && (
          <span className="rack__cap">
            {fmtBytes(total - free)} / {fmtBytes(total)} used
          </span>
        )}
      </header>
      {nodes.map((n) => (
        <div className="node" key={n.id}>
          <div className="node__head">
            <span className="node__id">{n.id}</span>
            {n.addr && <span className="node__addr">{n.addr}</span>}
            <span className="node__live">
              <NodeLive n={n} />
            </span>
          </div>
          {n.disks.map((d) => (
            <Disk d={d} key={d.id} />
          ))}
        </div>
      ))}
    </section>
  );
}

// Migrations is the schema panel: where the cluster's schema stands against
// this binary's, and — once every node runs the new binary — the control that
// applies what is pending, cluster-wide under the migrate election.
function Migrations() {
  const qc = useQueryClient();
  const toast = useToast();
  const q = useGetMigrationStatus({ query: { refetchInterval: 10000 } });

  const apply = useApplyMigrations({
    mutation: {
      onSuccess: (st) => {
        toast.notify(
          st.last_applied?.length
            ? `Migrated to schema v${st.cluster_schema_version}`
            : "Nothing to migrate",
          "success",
        );
        void qc.invalidateQueries({
          queryKey: getGetMigrationStatusQueryKey(),
        });
      },
      onError: (err) => toast.notify(err.error_message, "error"),
    },
  });

  const m = q.data;
  if (!m || m.state === "disabled") return null;

  const joined = m.cluster_schema_version > 0;

  return (
    <>
      <div className="section-title">Schema</div>
      <div className="card">
        <h2>
          Migrations
          <span className="sub">
            cluster v{joined ? m.cluster_schema_version : "—"} · binary v
            {m.binary_schema_version}
          </span>
        </h2>

        {m.up_to_date ? (
          <p className="muted-note">
            Every schema migration this binary knows about has been applied.
          </p>
        ) : !joined ? (
          <p className="muted-note">
            No schema version is recorded yet — no node has joined this cluster.
          </p>
        ) : (
          <>
            <p className="muted-note">
              {m.pending.length} pending{" "}
              {m.pending.length === 1 ? "migration" : "migrations"}. Apply once
              a rolling upgrade has replaced every node's binary; until then the
              cluster keeps operating at its current schema.
            </p>
            <ul className="migrations">
              {m.pending.map((p) => (
                <li key={p.version}>
                  <span className="chip">v{p.version}</span> {p.description}
                </li>
              ))}
            </ul>
          </>
        )}

        {m.last_error && <div className="err-box">{m.last_error}</div>}

        <div className="actions" style={{ marginTop: "14px" }}>
          <button
            type="button"
            className="primary"
            onClick={() => apply.mutate()}
            disabled={m.running || apply.isPending || m.pending.length === 0}
          >
            {m.running || apply.isPending ? "Migrating…" : "Apply migrations"}
          </button>
        </div>
      </div>
    </>
  );
}

export default function Cluster() {
  const q = useGetClusterStatus({ query: { refetchInterval: 5000 } });

  if (q.isLoading) return <div className="empty">Loading cluster status…</div>;
  if (q.error) return <div className="err-box">{q.error.error_message}</div>;

  const c = q.data;
  if (!c) return null;

  if (c.state === "disabled") {
    return (
      <>
        <div className="section-title">Cluster</div>
        <div className="notice">
          <h2>Cluster mode is off</h2>
          <p>
            This instance serves a single filesystem backend. Cluster status —
            nodes, disks and placement — appears here when it runs with{" "}
            <code>storage.type: cluster</code>.
          </p>
        </div>
      </>
    );
  }

  // Group nodes by failure domain (rack); compute the worst disk for the
  // overall health pill and the aggregate fill for the capacity bar.
  const byRack: Record<string, ClusterNode[]> = {};
  for (const n of c.nodes) (byRack[n.rack || "unlabeled"] ??= []).push(n);
  const rackIds = Object.keys(byRack).sort();

  const usedFrac = c.total_bytes > 0 ? 1 - c.free_bytes / c.total_bytes : 0;
  const usedBand = band(usedFrac);
  // Count disks past the drain watermark — a capacity concern distinct from
  // the cluster's availability, so it rides its own alert rather than flipping
  // the operational pill.
  let nearFull = 0;
  for (const n of c.nodes)
    for (const d of n.disks)
      if ((d.total_bytes ?? 0) > 0 && (d.fullness ?? 0) >= 0.9) nearFull++;
  const schemaSkew = c.schema_version !== c.binary_schema_version;

  return (
    <>
      <div className="cluster-head">
        <span className="pill healthy">Operational</span>
        {nearFull > 0 && (
          <span
            className="pill critical"
            title="Disks at or above the 0.9 drain watermark. Add capacity or lower a full disk's weight."
          >
            {nearFull} {nearFull === 1 ? "disk" : "disks"} over 90%
          </span>
        )}
        {c.nodes_not_reporting > 0 && (
          <span
            className="pill degraded"
            title="Nodes that did not answer the live-state request: unreachable, or running a binary that does not serve it. Their capacity and placement still come from the control plane."
          >
            {c.nodes_not_reporting} of {c.node_count} not reporting
          </span>
        )}
        {schemaSkew ? (
          <span
            className="pill degraded"
            title="The cluster's schema differs from this binary's — an upgrade is in progress or a node is behind."
          >
            schema v{c.schema_version} → v{c.binary_schema_version}
          </span>
        ) : (
          <span className="chip">schema v{c.schema_version}</span>
        )}
        <span className={`chip ${c.rebalance_running ? "on" : ""}`}>
          {c.rebalance_running ? "rebalancing" : "rebalance idle"}
          {c.rebalance_running && c.rebalance_cursor_bucket
            ? ` · at ${c.rebalance_cursor_bucket}/${c.rebalance_cursor_key ?? ""}`
            : ""}
        </span>
      </div>

      <div className="readout">
        <div>
          <div className="n">{c.node_count}</div>
          <div className="l">Nodes</div>
        </div>
        <div>
          <div className="n">{c.disk_count}</div>
          <div className="l">Disks</div>
        </div>
        <div>
          <div className="n">{rackIds.length}</div>
          <div className="l">Failure domains</div>
        </div>
        <div style={{ flexGrow: 2 }}>
          <div className="n">
            {fmtBytes(c.total_bytes - c.free_bytes)}{" "}
            <small>/ {fmtBytes(c.total_bytes)}</small>
          </div>
          <div className="l">Capacity used</div>
          <div className={`capbar ${usedBand}`}>
            <span style={{ width: `${Math.max(usedFrac * 100, 0.5)}%` }} />
          </div>
        </div>
        <div>
          <div className="n">{Math.round(c.placement_skew * 100)}%</div>
          <div
            className="l"
            title="Max minus min disk fullness across the cluster."
          >
            Placement skew
          </div>
        </div>
        <div>
          <div className={`n ${c.repair_queue_depth > 0 ? "degraded" : ""}`}>
            {c.repair_queue_depth}
          </div>
          <div
            className="l"
            title="Objects with pending async replication/repair work, summed over the nodes that reported."
          >
            Repair queue
          </div>
        </div>
      </div>

      <div className="section-title">Failure domains</div>
      {rackIds.map((id) => (
        <Rack id={id} nodes={byRack[id]} key={id} />
      ))}

      <Migrations />
    </>
  );
}
