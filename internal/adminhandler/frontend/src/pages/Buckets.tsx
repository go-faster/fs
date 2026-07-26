import { useGetBucketUsage } from "../api/admin";
import type { BucketUsage } from "../api/model";

// Binary byte units, matching the sizing docs and the cluster page.
function fmtBytes(n: number): string {
  if (n <= 0) return "0 B";
  const u = ["B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"];
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), u.length - 1);
  const v = n / 1024 ** i;
  return `${i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)} ${u[i]}`;
}

// fmtAge renders how long ago a timestamp was, since what an operator wants
// from these is freshness — "recounted 3h ago" — not a wall-clock date.
function fmtAge(iso?: string): string {
  if (!iso) return "never";

  const ms = Date.now() - new Date(iso).getTime();
  if (!Number.isFinite(ms) || ms < 0) return "just now";

  const m = Math.floor(ms / 60000);
  if (m < 1) return "just now";
  if (m < 60) return `${m}m ago`;

  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;

  return `${Math.floor(h / 24)}d ago`;
}

function Row({ b }: { b: BucketUsage }) {
  return (
    <tr>
      <td>{b.bucket}</td>
      <td>{b.objects.toLocaleString()}</td>
      <td>{fmtBytes(b.bytes)}</td>
      <td
        className={b.counted ? "" : "muted"}
        title={
          b.counted
            ? "When a full recount last verified this total against the objects themselves."
            : "No recount has verified this total yet; it has only ever been maintained incrementally."
        }
      >
        {fmtAge(b.counted)}
      </td>
    </tr>
  );
}

export default function Buckets() {
  const usage = useGetBucketUsage();

  const buckets = usage.data?.buckets ?? [];

  return (
    <>
      <div className="section-title">Buckets</div>

      <div className="card">
        <h2>Usage</h2>

        {usage.isLoading && <div className="empty">Loading…</div>}
        {usage.error && (
          <div className="err-box">{usage.error.error_message}</div>
        )}

        {usage.data && buckets.length === 0 && (
          <div className="empty">No bucket has been accounted for yet.</div>
        )}

        {buckets.length > 0 && (
          <>
            <table className="left">
              <thead>
                <tr>
                  <th>Bucket</th>
                  <th>Objects</th>
                  <th>Size</th>
                  <th>Recounted</th>
                </tr>
              </thead>
              <tbody>
                {buckets.map((b) => (
                  <Row key={b.bucket} b={b} />
                ))}
              </tbody>
            </table>

            <dl className="kv">
              <dt>Total objects</dt>
              <dd>{(usage.data?.objects ?? 0).toLocaleString()}</dd>
              <dt>Total size</dt>
              <dd>{fmtBytes(usage.data?.bytes ?? 0)}</dd>
            </dl>

            {/* Logical size, so an operator does not read it as disk usage:
                replication and erasure coding multiply it on the way down. */}
            <p className="lead">
              Sizes are logical — what clients stored. What the cluster holds on
              disk is this multiplied by the bucket's replication scheme.
            </p>
          </>
        )}
      </div>
    </>
  );
}
