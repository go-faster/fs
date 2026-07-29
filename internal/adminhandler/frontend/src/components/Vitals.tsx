import type { ReactNode } from "react";
import { useGetBucketUsage, useGetClusterStatus, useGetInfo, useListAccessKeys } from "../api/admin";
import { band, type Band } from "./ui";
import { fmtBytes, fmtDur, fmtNum, pct } from "../lib/format";
import { CLUSTER_POLL, INFO_POLL, USAGE_POLL } from "../lib/poll";

const DASH = "—";

/** A cell reads red or amber only when the reading itself is the problem. */
type Tone = "normal" | "warning" | "danger";

const BAND_TONE: Record<Band, Tone> = {
  healthy: "normal",
  degraded: "warning",
  critical: "danger",
};

function Cell({
  label,
  value,
  unit,
  tone = "normal",
  title,
}: {
  label: string;
  value: ReactNode;
  unit?: string;
  tone?: Tone;
  title?: string;
}) {
  return (
    <div className="vitals__cell" title={title}>
      <span className="vitals__label">{label}</span>
      <div className="vitals__reading">
        <span className={`vitals__value${tone === "normal" ? "" : ` vitals__value_${tone}`}`}>
          {value}
          {unit ? <span className="vitals__unit">{unit}</span> : null}
        </span>
      </div>
    </div>
  );
}

/**
 * The instrument tape: the readings that answer "what is this instance doing
 * right now", kept visible on every route. Values come from the polls the pages
 * make anyway — react-query serves both from one request per interval.
 *
 * A single-node instance has no cluster to report on and its usage index is not
 * served, so the tape gives what it honestly can and ends there rather than
 * padding itself out with placeholders.
 */
export function Vitals() {
  const info = useGetInfo({ query: { refetchInterval: INFO_POLL } });
  const cluster = useGetClusterStatus({ query: { refetchInterval: CLUSTER_POLL } });
  const usage = useGetBucketUsage({ query: { refetchInterval: USAGE_POLL, retry: false } });
  const keys = useListAccessKeys({ query: { refetchInterval: INFO_POLL } });

  const c = cluster.data;
  const clustered = c?.state === "ok";
  // Until the first cluster reply lands there is no way to tell a single-node
  // instance from a cluster, so the tape shows only what it already knows
  // rather than flashing the wrong set of cells for one poll.
  const known = !cluster.isLoading;

  const usedRatio = c && c.total_bytes > 0 ? 1 - c.free_bytes / c.total_bytes : 0;
  const capacityBand = band(usedRatio);

  return (
    <div className="vitals" role="group" aria-label="Instance vitals">
      <Cell label="uptime" value={info.data ? fmtDur(info.data.uptime_seconds) : DASH} />

      {clustered && c ? (
        <>
          <Cell
            label="nodes"
            value={`${c.nodes_reporting}/${c.node_count}`}
            tone={c.nodes_not_reporting > 0 ? "warning" : "normal"}
            title={
              c.nodes_not_reporting > 0
                ? `${c.nodes_not_reporting} node(s) did not answer the live-state request: unreachable, or running a binary that does not serve it.`
                : "Every node answered the live-state request."
            }
          />
          <Cell
            label="capacity"
            // The unit slot is for a dimension the number is measured in, set
            // smaller so it does not compete — a percent sign is part of the
            // reading, not a dimension, so it stays the size of its digits.
            value={c.total_bytes > 0 ? `${Math.round(pct(usedRatio))}%` : DASH}
            tone={BAND_TONE[capacityBand]}
            title={
              c.total_bytes > 0
                ? `${fmtBytes(c.total_bytes - c.free_bytes)} used of ${fmtBytes(c.total_bytes)}.`
                : "No node has reported disk capacity."
            }
          />
          <Cell
            label="repair queue"
            value={fmtNum(c.repair_queue_depth)}
            tone={c.repair_queue_depth > 0 ? "warning" : "normal"}
            title="Objects with pending async replication/repair work, summed over the nodes that reported."
          />
          <Cell
            label="objects"
            value={usage.data?.objects == null ? DASH : fmtNum(usage.data.objects)}
            title="Objects across every accounted bucket, from the cluster's durable usage index."
          />
        </>
      ) : known ? (
        <Cell
          label="access keys"
          value={keys.data ? fmtNum(keys.data.keys.length) : DASH}
          title="Credentials this instance will accept, from config and from the runtime store."
        />
      ) : null}
    </div>
  );
}
