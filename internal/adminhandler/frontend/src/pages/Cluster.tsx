import { useGetClusterStatus } from "../api/admin";
import type { ClusterDisk, ClusterNode } from "../api/model";

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
      <span className={`disk__w ${drained ? "drained" : ""}`}>
        {drained ? "drain" : `w${d.weight}`}
      </span>
      <span className="disk__cap">
        {known
          ? `${fmtBytes((d.total_bytes ?? 0) - (d.free_bytes ?? 0))} / ${fmtBytes(d.total_bytes ?? 0)}`
          : "no data"}
      </span>
    </div>
  );
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
          </div>
          {n.disks.map((d) => (
            <Disk d={d} key={d.id} />
          ))}
        </div>
      ))}
    </section>
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
      </div>

      <div className="section-title">Failure domains</div>
      {rackIds.map((id) => (
        <Rack id={id} nodes={byRack[id]} key={id} />
      ))}
    </>
  );
}
