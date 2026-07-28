// Formatting helpers shared across pages.

const DASH = "—";

/**
 * Binary byte units, matching the sizing docs (TiB/GiB, not TB/GB). Capacity in
 * this console always means what the filesystem reports, so it is always binary.
 */
export function fmtBytes(n: number | null | undefined): string {
  if (n == null) return DASH;
  let v = Number(n);
  const neg = v < 0;
  v = Math.abs(v);
  const u = ["B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"];
  let i = 0;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return (neg ? "-" : "") + v.toFixed(i ? 1 : 0) + " " + u[i];
}

export function fmtNum(n: number | null | undefined): string {
  return n == null ? DASH : Number(n).toLocaleString();
}

export function fmtTime(s: string | null | undefined): string {
  return s ? new Date(s).toLocaleString() : DASH;
}

export function fmtDur(secondsInput: number): string {
  const sec = Math.floor(secondsInput);
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  const parts = [d && d + "d", h && h + "h", m && m + "m", s + "s"].filter(Boolean) as string[];
  return parts.slice(0, 2).join(" ") || "0s";
}

/**
 * How long ago something happened. Freshness is what an operator wants from
 * these timestamps — "recounted 3h ago", not a wall-clock date — and a cycle
 * falling behind shows up as the age receding.
 */
export function fmtAge(iso: string | null | undefined): string {
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

export function pct(ratio: number): number {
  return Math.max(0, Math.min(100, ratio * 100));
}
