import { Alert, Col, Flex, Icon, Row, Text } from "@gravity-ui/uikit";
import type { ColProps } from "@gravity-ui/uikit";
import { CircleCheck } from "@gravity-ui/icons";
import { useGetClusterStatus, useGetInfo, useListAccessKeys } from "../api/admin";
import type { ClusterStatus } from "../api/model";
import { DRAIN_WATERMARK, KV, Mono, Panel, QueryState, Rule } from "../components/ui";
import { fmtDur, fmtNum, fmtTime } from "../lib/format";
import { CLUSTER_POLL, INFO_POLL } from "../lib/poll";

// Build has eight facts to the credential counts' three, so the columns are
// proportioned to what they carry rather than split down the middle.
const BUILD_COL: ColProps["size"] = [12, { m: 7 }];
const KEYS_COL: ColProps["size"] = [12, { m: 5 }];

/** Disks at or above the drain watermark, across every node that reported one. */
function countNearFull(c: ClusterStatus): number {
  let n = 0;
  for (const node of c.nodes) {
    for (const d of node.disks) {
      if ((d.total_bytes ?? 0) > 0 && (d.fullness ?? 0) >= DRAIN_WATERMARK) n++;
    }
  }
  return n;
}

/**
 * What needs attention. Nothing renders when the cluster is clean beyond a
 * single line — the presence of this block is the signal, so it stays quiet
 * until there is something to say.
 */
function Attention({ cluster }: { cluster: ClusterStatus }) {
  const nearFull = countNearFull(cluster);
  const schemaSkew = cluster.schema_version !== cluster.binary_schema_version;
  const notReporting = cluster.nodes_not_reporting;

  if (!nearFull && !schemaSkew && !notReporting) {
    return (
      <Flex alignItems="center" gap={2}>
        <Icon data={CircleCheck} size={14} color="positive" />
        <Text variant="body-1" color="secondary">
          All {cluster.node_count} nodes reporting, {cluster.disk_count} disks under the drain
          watermark
        </Text>
      </Flex>
    );
  }

  return (
    <Flex direction="column" gap={2}>
      {notReporting > 0 && (
        <Alert
          theme="warning"
          view="outlined"
          title={`${notReporting} of ${cluster.node_count} nodes are not reporting`}
          message="They did not answer the live-state request: unreachable, or running a binary that does not serve it. Their capacity and placement still come from the control plane."
        />
      )}
      {nearFull > 0 && (
        <Alert
          theme="danger"
          view="outlined"
          title={`${nearFull} ${nearFull === 1 ? "disk is" : "disks are"} over 90% full`}
          message="At the drain watermark placement stops choosing a disk. Add capacity, or lower a full disk's weight to move its data off."
        />
      )}
      {schemaSkew && (
        <Alert
          theme="warning"
          view="outlined"
          title={`Cluster schema v${cluster.schema_version}, binary schema v${cluster.binary_schema_version}`}
          message="An upgrade is in progress or a node is behind. Apply the pending migrations from Cluster → Status once every node runs the new binary."
        />
      )}
    </Flex>
  );
}

export default function Overview() {
  const info = useGetInfo({ query: { refetchInterval: INFO_POLL } });
  const keys = useListAccessKeys({ query: { refetchInterval: INFO_POLL } });
  const cluster = useGetClusterStatus({ query: { refetchInterval: CLUSTER_POLL } });

  const clustered = cluster.data?.state === "ok";

  const managed = keys.data?.keys.filter((k) => k.source === "managed").length ?? 0;
  const fromConfig = (keys.data?.keys.length ?? 0) - managed;

  return (
    <Flex direction="column" gap={5}>
      {clustered && cluster.data && <Attention cluster={cluster.data} />}

      {/* Build facts and credential counts are one subject — what this instance
          is — so they share one card. Two cards made a three-row list stand as
          tall as an eight-row one and left half a box empty. */}
      <Flex direction="column" gap={3}>
        <Rule>Instance</Rule>
        <Panel>
          <Row space="4" spaceRow="5">
            <Col size={BUILD_COL}>
              <Flex direction="column" gap={2}>
                <span className="label-micro">build</span>
                <QueryState query={info} what="instance info">
                  {(data) => (
                    <KV
                      rows={[
                        ["version", <Mono>{data.version || "dev"}</Mono>],
                        ["commit", <Mono>{(data.commit || "—").slice(0, 12)}</Mono>],
                        ["go", <Mono>{data.go_version}</Mono>],
                        ["platform", <Mono>{`${data.os}/${data.arch}`}</Mono>],
                        // Scoped to this process on purpose: a headless `fs admin`
                        // serves no S3, so it signs nothing even in a cluster
                        // whose storage nodes do.
                        [
                          "request signing",
                          <Mono>{data.auth_enabled ? "SigV4" : "not served here"}</Mono>,
                        ],
                        ["started", <Mono>{fmtTime(data.start_time)}</Mono>],
                        ["uptime", <Mono>{fmtDur(data.uptime_seconds)}</Mono>],
                        // An orchestrator writes a revision into each node's
                        // config and reads it back here to confirm the node has
                        // applied it, so an empty one is worth saying out loud.
                        ["config revision", <Mono>{data.config_revision || "not set"}</Mono>],
                      ]}
                    />
                  )}
                </QueryState>
              </Flex>
            </Col>

            <Col size={KEYS_COL}>
              <Flex direction="column" gap={2}>
                <span className="label-micro">credentials</span>
                <QueryState query={keys} what="access keys">
                  {(data) => (
                    <KV
                      rows={[
                        ["total", <Mono>{fmtNum(data.keys.length)}</Mono>],
                        ["runtime-managed", <Mono>{fmtNum(managed)}</Mono>],
                        ["from config", <Mono>{fmtNum(fromConfig)}</Mono>],
                      ]}
                    />
                  )}
                </QueryState>
              </Flex>
            </Col>
          </Row>
        </Panel>
      </Flex>
    </Flex>
  );
}
