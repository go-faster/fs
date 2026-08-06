import { useQueryClient } from "@tanstack/react-query";
import { Alert, Button, Col, Flex, Label, Row, Text, useToaster } from "@gravity-ui/uikit";
import type { ColProps } from "@gravity-ui/uikit";
import {
  getGetMigrationStatusQueryKey,
  useApplyMigrations,
  useGetClusterStatus,
  useGetMigrationStatus,
} from "../api/admin";
import type { ClusterDisk, ClusterNode, ClusterNodeLive, ClusterStatus } from "../api/model";
import {
  band,
  Chip,
  DRAIN_WATERMARK,
  errText,
  ErrorAlert,
  KV,
  Loading,
  Mono,
  Panel,
  Rule,
  UsageBar,
  type Band,
} from "../components/ui";
import { fmtAge, fmtBytes, fmtNum } from "../lib/format";
import { MetadataPlane } from "../components/MetadataPlane";
import { CLUSTER_POLL, MIGRATION_POLL } from "../lib/poll";

const HALF: ColProps["size"] = [12, { l: 6 }];

/**
 * A drained disk is out of placement, but that is not the same as being safe to
 * pull. Weight says placement stopped choosing it; `has_data` says whether its
 * fragments have actually moved off, which is what makes removing it safe; and
 * bytes says how far along the move is.
 */
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
      : `Draining: ${fmtNum(d.fragments)} fragments left, ${fmtBytes(d.bytes ?? 0)}.`;
  }
  return undefined;
}

/**
 * One rung of the ladder: five grid cells, not a box. The rack owns the grid,
 * so every disk under every node in the failure domain shares one set of
 * columns and the occupancy bars line up straight down the page.
 */
function DiskRow({ d }: { d: ClusterDisk }) {
  const known = (d.total_bytes ?? 0) > 0;
  const drained = d.weight <= 0;
  const fullness = d.fullness ?? 0;
  const b: Band = band(fullness);

  const state = drained ? "drained" : known ? b : "unknown";
  const percent = Math.round(fullness * 100);

  return (
    <>
      <span className="ladder__id">{d.id}</span>
      <div
        className={`meter meter_${state}`}
        role="meter"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={known ? percent : undefined}
        aria-label={`disk ${d.id} ${known ? `${percent}% full` : "capacity unknown"}`}
      >
        {known && !drained && (
          // A disk with a sliver of data still gets a visible mark: an invisible
          // bar reads as an empty disk, which is a different fact.
          <span className="meter__fill" style={{ width: `${Math.max(fullness * 100, 1.5)}%` }} />
        )}
        {known && (
          <span
            className="meter__mark"
            style={{ insetInlineStart: `${DRAIN_WATERMARK * 100}%` }}
          />
        )}
      </div>
      <span className={`ladder__num ${known ? `text-${b}` : "text-muted"}`}>
        {known ? `${percent}%` : "—"}
      </span>
      <span className={`ladder__id ${drained ? "text-muted" : ""}`} title={drainTitle(d)}>
        {drained ? drainLabel(d) : `w${d.weight}`}
      </span>
      <span className="ladder__cap">
        {known
          ? `${fmtBytes((d.total_bytes ?? 0) - (d.free_bytes ?? 0))} / ${fmtBytes(d.total_bytes ?? 0)}`
          : "no data"}
      </span>
    </>
  );
}

/**
 * How stale this node's scrub coverage is. Counts of scrub work say how busy
 * the scrubber was; this says whether it is keeping up.
 */
function coverageLabel(l: ClusterNodeLive): string {
  const never = l.never_verified ?? 0;
  if (never > 0) return `unverified ${fmtNum(never)}`;
  if (!l.oldest_verified) return "unverified";
  return `verified ${fmtAge(l.oldest_verified)}`;
}

function coverageTitle(l: ClusterNodeLive): string {
  const held = fmtNum(l.objects_held ?? 0);
  const never = l.never_verified ?? 0;
  if (never > 0) {
    return `${fmtNum(never)} of ${held} objects on this node have never been verified.`;
  }
  return `All ${held} objects on this node have been verified since ${l.oldest_verified}.`;
}

/**
 * What only the node itself knows — its queues, runner and scrub totals. A node
 * that did not answer says so, with the reason: an empty live row would
 * otherwise read as a healthy, idle node.
 */
function NodeLive({ n }: { n: ClusterNode }) {
  if (!n.live) {
    return (
      <Label
        theme="warning"
        size="xs"
        title={n.live_error ?? "The node did not report its live state."}
      >
        not reporting
      </Label>
    );
  }

  const l = n.live;
  const rebalancing = l.rebalance_state === "running" || l.rebalance_state === "waiting";

  return (
    <>
      <Label
        theme={l.repair_queue_depth > 0 ? "info" : "unknown"}
        size="xs"
        title="Objects with pending async replication/repair work on this node."
      >
        queue {fmtNum(l.repair_queue_depth)}
      </Label>
      {rebalancing && (
        <Label theme="info" size="xs">
          {l.rebalance_state} · {fmtNum(l.rebalance_relocated)}/{fmtNum(l.rebalance_objects)}
        </Label>
      )}
      {l.rebuilt_fragments > 0 && (
        <Label theme="unknown" size="xs" title="Fragments rebuilt by scrubs on this node.">
          rebuilt {fmtNum(l.rebuilt_fragments)}
        </Label>
      )}
      {l.corrupt_replicas > 0 && (
        <Label
          theme="danger"
          size="xs"
          title="Replica payloads that failed checksum verification (bit-rot)."
        >
          corrupt {fmtNum(l.corrupt_replicas)}
        </Label>
      )}
      {l.objects_held !== undefined && (
        <Label
          theme={(l.never_verified ?? 0) > 0 ? "warning" : "unknown"}
          size="xs"
          title={coverageTitle(l)}
        >
          {coverageLabel(l)}
        </Label>
      )}
      {l.ec_unverified && (
        <Label
          theme="danger"
          size="xs"
          title="The node's last scrub pass saw an EC set failing parity verification."
        >
          EC unverified
        </Label>
      )}
      {l.version && (
        <Text variant="caption-2" color="hint" title="The binary this node is running.">
          {l.version}
        </Text>
      )}
    </>
  );
}

/**
 * A failure domain, as placement actually counts them.
 *
 * `Node.FailureDomain()` (internal/cluster/topology.go) keys a node by its rack
 * when it has one, and by the node itself when it does not — "an unlabeled node
 * shares fate with nobody". So unlabeled nodes are not one domain between them;
 * they are one domain each, and a copy landing on two of them is as spread as
 * placement can make it. Drawing them as a single box would overstate the
 * blast radius of losing one, which is the opposite of what this page is for.
 */
interface Domain {
  /** Rack label, or undefined for the nodes that carry none. */
  rack?: string;
  nodes: ClusterNode[];
}

function groupByDomain(nodes: ClusterNode[]): Domain[] {
  const racks = new Map<string, ClusterNode[]>();
  const unlabeled: ClusterNode[] = [];

  for (const n of nodes) {
    if (n.rack) {
      const bucket = racks.get(n.rack);
      if (bucket) bucket.push(n);
      else racks.set(n.rack, [n]);
    } else {
      unlabeled.push(n);
    }
  }

  const out: Domain[] = [...racks.keys()]
    .sort()
    .map((rack) => ({ rack, nodes: racks.get(rack)! }));

  // Kept in one panel rather than one per node: the grouping is presentational
  // (they are still a domain each, which the caption says), and N identical
  // panels would drown the racks that do carry a label.
  if (unlabeled.length > 0) out.push({ nodes: unlabeled });

  return out;
}

/** How many domains placement can spread across: one per rack, one per unlabeled node. */
function countDomains(nodes: ClusterNode[]): number {
  const racks = new Set<string>();
  let unlabeled = 0;
  for (const n of nodes) {
    if (n.rack) racks.add(n.rack);
    else unlabeled++;
  }
  return racks.size + unlabeled;
}

function DomainPanel({ domain }: { domain: Domain }) {
  const { rack, nodes } = domain;
  let total = 0;
  let free = 0;
  let diskCount = 0;

  for (const n of nodes) {
    for (const d of n.disks) {
      diskCount++;
      if ((d.total_bytes ?? 0) > 0) {
        total += d.total_bytes ?? 0;
        free += d.free_bytes ?? 0;
      }
    }
  }

  const disks = `${diskCount} ${diskCount === 1 ? "disk" : "disks"}`;

  return (
    <Panel
      title={rack ? <Mono>{rack}</Mono> : "No rack label"}
      sub={
        rack
          ? `${nodes.length} ${nodes.length === 1 ? "node" : "nodes"} · ${disks}`
          : `${nodes.length} ${nodes.length === 1 ? "node, its" : "nodes, each its"} own domain · ${disks}`
      }
      actions={
        total > 0 ? (
          <Text variant="code-inline-1" color="secondary">
            {fmtBytes(total - free)} / {fmtBytes(total)}
          </Text>
        ) : undefined
      }
      scroll
    >
      {/* Node headers and disk rungs are siblings in one grid, not nested
          boxes — that is what keeps the columns aligned across every node. */}
      <div className="ladder">
        {nodes.flatMap((n) => [
          <div className="ladder__node" key={n.id}>
            <Mono>{n.id}</Mono>
            {n.addr && (
              <Text variant="caption-2" color="secondary">
                {n.addr}
              </Text>
            )}
            <Flex gap={1} wrap alignItems="center">
              <NodeLive n={n} />
            </Flex>
          </div>,
          ...(n.disks.length === 0
            ? [
                <Text key={`${n.id}-empty`} className="ladder__span" variant="body-1" color="hint">
                  This node registered no disks.
                </Text>,
              ]
            : n.disks.map((d) => <DiskRow key={`${n.id}/${d.id}`} d={d} />)),
        ])}
      </div>
    </Panel>
  );
}

/**
 * Where the cluster's schema stands against this binary's, and — once every
 * node runs the new binary — the control that applies what is pending,
 * cluster-wide under the migrate election.
 */
function Migrations() {
  const qc = useQueryClient();
  const toaster = useToaster();
  const q = useGetMigrationStatus({ query: { refetchInterval: MIGRATION_POLL } });

  const apply = useApplyMigrations({
    mutation: {
      onSuccess: (st) => {
        toaster.add({
          name: "apply-migrations",
          theme: "success",
          title: st.last_applied?.length ? "Schema migrated" : "Nothing to migrate",
          content: st.last_applied?.length
            ? `The cluster is now at schema v${st.cluster_schema_version}.`
            : "The cluster was already at this binary's schema version.",
        });
        void qc.invalidateQueries({ queryKey: getGetMigrationStatusQueryKey() });
      },
      onError: (err) =>
        toaster.add({
          name: "apply-migrations",
          theme: "danger",
          title: "Migration failed",
          content: errText(err),
          autoHiding: false,
        }),
    },
  });

  const m = q.data;
  if (!m || m.state === "disabled") return null;

  const joined = m.cluster_schema_version > 0;

  return (
    <Flex direction="column" gap={3}>
      <Rule>Schema</Rule>
      <Panel
        title="Migrations"
        sub={`cluster v${joined ? m.cluster_schema_version : "—"} · binary v${m.binary_schema_version}`}
        actions={
          <Button
            view="action"
            onClick={() => apply.mutate()}
            loading={m.running || apply.isPending}
            disabled={m.pending.length === 0}
          >
            Apply migrations
          </Button>
        }
      >
        <Flex direction="column" gap={4}>
          {m.up_to_date ? (
            <Text variant="body-1" color="secondary">
              Every schema migration this binary knows about has been applied.
            </Text>
          ) : !joined ? (
            <Text variant="body-1" color="secondary">
              No schema version is recorded yet — no node has joined this cluster.
            </Text>
          ) : (
            <>
              <Text variant="body-1" color="secondary">
                {m.pending.length} pending {m.pending.length === 1 ? "migration" : "migrations"}.
                Apply once a rolling upgrade has replaced every node's binary; until then the
                cluster keeps operating at its current schema.
              </Text>
              <Flex direction="column" gap={2}>
                {m.pending.map((p) => (
                  <Flex key={p.version} alignItems="baseline" gap={2}>
                    <Label theme="info" size="xs">
                      v{p.version}
                    </Label>
                    <Text variant="body-1">{p.description}</Text>
                  </Flex>
                ))}
              </Flex>
            </>
          )}

          {m.last_error && (
            <Alert
              theme="danger"
              view="outlined"
              title="The last migration run failed"
              message={m.last_error}
            />
          )}
        </Flex>
      </Panel>
    </Flex>
  );
}

/** The one-line verdict, then the specific concerns that qualify it. */
function StatusPills({ c, nearFull }: { c: ClusterStatus; nearFull: number }) {
  const schemaSkew = c.schema_version !== c.binary_schema_version;
  const degraded = c.nodes_not_reporting > 0;

  return (
    <Flex gap={2} wrap alignItems="center">
      <Label theme={degraded ? "warning" : "success"}>
        {degraded ? "Degraded" : "Operational"}
      </Label>

      {c.nodes_not_reporting > 0 && (
        <Label
          theme="warning"
          title="Nodes that did not answer the live-state request: unreachable, or running a binary that does not serve it. Their capacity and placement still come from the control plane."
        >
          {c.nodes_not_reporting} of {c.node_count} not reporting
        </Label>
      )}

      {nearFull > 0 && (
        <Label
          theme="danger"
          title="Disks at or above the 0.9 drain watermark. Add capacity, or lower a full disk's weight."
        >
          {nearFull} {nearFull === 1 ? "disk" : "disks"} over 90%
        </Label>
      )}

      {schemaSkew ? (
        <Label
          theme="warning"
          title="The cluster's schema differs from this binary's — an upgrade is in progress, or a node is behind."
        >
          schema v{c.schema_version} → v{c.binary_schema_version}
        </Label>
      ) : (
        <Chip>schema v{c.schema_version}</Chip>
      )}

      <Chip on={c.rebalance_running}>
        {c.rebalance_running ? "rebalancing" : "rebalance idle"}
      </Chip>
    </Flex>
  );
}

export default function Cluster() {
  const q = useGetClusterStatus({ query: { refetchInterval: CLUSTER_POLL } });

  if (q.isLoading) return <Loading />;
  if (q.error) return <ErrorAlert error={q.error} what="cluster status" />;

  const c = q.data;
  if (!c) return null;

  if (c.state === "disabled") {
    return (
      <Flex direction="column" gap={3}>
        <Rule>Cluster</Rule>
        <Panel title="Cluster mode is off" sub="single filesystem backend">
          <Text variant="body-1" color="secondary">
            This instance serves one filesystem backend. Nodes, disks and placement appear here
            when it runs with <Mono>storage.type: cluster</Mono>.
          </Text>
        </Panel>
      </Flex>
    );
  }

  // Placement's whole job is to spread a scheme across failure domains, so this
  // is the grouping the page leads with.
  const domains = groupByDomain(c.nodes);
  const domainCount = countDomains(c.nodes);

  const usedRatio = c.total_bytes > 0 ? 1 - c.free_bytes / c.total_bytes : 0;

  // Disks past the drain watermark are a capacity concern distinct from the
  // cluster's availability, so they ride their own pill rather than flipping
  // the operational verdict.
  let nearFull = 0;
  for (const n of c.nodes) {
    for (const d of n.disks) {
      if ((d.total_bytes ?? 0) > 0 && (d.fullness ?? 0) >= DRAIN_WATERMARK) nearFull++;
    }
  }

  const cursor =
    c.rebalance_running && c.rebalance_cursor_bucket
      ? `${c.rebalance_cursor_bucket}/${c.rebalance_cursor_key ?? ""}`
      : null;

  return (
    <Flex direction="column" gap={5}>
      <StatusPills c={c} nearFull={nearFull} />

      {/* The ladder leads. Where the data sits and how full each disk is are
          what this page exists to answer; the aggregates below are the
          footnotes to it, not the other way round. */}
      <Flex direction="column" gap={3}>
        <Rule>Failure domains</Rule>
        {domains.map((d) => (
          <DomainPanel domain={d} key={d.rack ?? " unlabeled"} />
        ))}
      </Flex>

      {/* The aggregates are footnotes to the ladder, so they get one card, not
          two: a pair of half-width boxes gave a three-row list the same weight
          as the thing the page is about, and stretched its dotted leaders
          across the window to fill the space. */}
      <Flex direction="column" gap={3}>
        <Rule>Totals</Rule>
        <Panel>
          <Row space="4" spaceRow="4">
            <Col size={HALF}>
              <Flex direction="column" gap={4}>
                <UsageBar
                  label="cluster fill"
                  value={c.total_bytes > 0 ? `${Math.round(usedRatio * 100)}%` : "not reported"}
                  ratio={usedRatio}
                />
                <KV
                  rows={[
                    ["used", <Mono>{fmtBytes(c.total_bytes - c.free_bytes)}</Mono>],
                    ["free", <Mono>{fmtBytes(c.free_bytes)}</Mono>],
                    ["total", <Mono>{fmtBytes(c.total_bytes)}</Mono>],
                  ]}
                />
              </Flex>
            </Col>

            <Col size={HALF}>
              {/* Node count and repair queue live on the tape above, on every
                  route. What is left here is what the tape cannot say. */}
              <KV
                rows={[
                  [
                    "disks",
                    <Mono>{`${fmtNum(c.disk_count)} across ${domainCount} failure ${
                      domainCount === 1 ? "domain" : "domains"
                    }`}</Mono>,
                  ],
                  [
                    // Max minus min disk fullness: how unevenly the cluster is
                    // filled, which is what the rebalancer exists to shrink.
                    "skew",
                    <Mono>{`${Math.round(c.placement_skew * 100)}% fullest to emptiest disk`}</Mono>,
                  ],
                  [
                    "rebalance",
                    <Mono>
                      {cursor ? `at ${cursor}` : c.rebalance_running ? "running" : "idle"}
                    </Mono>,
                  ],
                ]}
              />
            </Col>
          </Row>
        </Panel>
      </Flex>

      <MetadataPlane />

      <Migrations />
    </Flex>
  );
}
