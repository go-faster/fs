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

  const managed = keys.data?.keys.filter((k) => k.source === "managed").length;

  return (
    <>
      <div className="section-title">Instance</div>
      <div className="grid">
        <div className="card">
          <h2>Build</h2>
          {info.isLoading && <div className="empty">Loading…</div>}
          {info.error && (
            <div className="err-box">{info.error.error_message}</div>
          )}
          {info.data && (
            <dl className="kv">
              <dt>Version</dt>
              <dd>{info.data.version}</dd>
              <dt>Commit</dt>
              <dd>{info.data.commit}</dd>
              <dt>Runtime</dt>
              <dd>
                {info.data.go_version} · {info.data.os}/{info.data.arch}
              </dd>
              <dt>Uptime</dt>
              <dd>{fmtUptime(info.data.uptime_seconds)}</dd>
              <dt>Auth</dt>
              <dd>{info.data.auth_enabled ? "enabled · SigV4" : "disabled"}</dd>
            </dl>
          )}
        </div>

        <div className="card">
          <h2>Access keys</h2>
          {keys.data ? (
            <dl className="kv">
              <dt>Total</dt>
              <dd>{keys.data.keys.length}</dd>
              <dt>Runtime-managed</dt>
              <dd>{managed}</dd>
              <dt>Config</dt>
              <dd>{keys.data.keys.length - (managed ?? 0)}</dd>
            </dl>
          ) : (
            <div className="empty">Loading…</div>
          )}
        </div>
      </div>
    </>
  );
}
