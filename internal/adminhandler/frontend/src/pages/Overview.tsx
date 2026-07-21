import { useGetInfo, useListAccessKeys } from "../api/admin";

function fmtUptime(seconds: number): string {
  const s = Math.floor(seconds);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const parts = [];
  if (d) parts.push(`${d}d`);
  if (h || d) parts.push(`${h}h`);
  parts.push(`${m}m`);
  return parts.join(" ");
}

export default function Overview() {
  const info = useGetInfo();
  const keys = useListAccessKeys();

  return (
    <>
      <div className="page-head">
        <h1>Overview</h1>
      </div>

      <div className="panel">
        <h2>Instance</h2>
        {info.isLoading && <p className="muted">Loading…</p>}
        {info.error && <p className="muted">Failed to load: {info.error.error_message}</p>}
        {info.data && (
          <dl className="kv">
            <dt>Version</dt>
            <dd className="mono">{info.data.version}</dd>
            <dt>Commit</dt>
            <dd className="mono">{info.data.commit}</dd>
            <dt>Runtime</dt>
            <dd className="mono">
              {info.data.go_version} · {info.data.os}/{info.data.arch}
            </dd>
            <dt>Uptime</dt>
            <dd>{fmtUptime(info.data.uptime_seconds)}</dd>
            <dt>Authentication</dt>
            <dd>{info.data.auth_enabled ? "enabled (SigV4)" : "disabled"}</dd>
          </dl>
        )}
      </div>

      <div className="panel">
        <h2>Access keys</h2>
        {keys.data ? (
          <p>
            <strong>{keys.data.keys.length}</strong> credential
            {keys.data.keys.length === 1 ? "" : "s"} configured (
            {keys.data.keys.filter((k) => k.source === "managed").length} managed
            at runtime).
          </p>
        ) : (
          <p className="muted">Loading…</p>
        )}
      </div>
    </>
  );
}
