import type { ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Button, Flex, Label, Table, Text, useToaster } from "@gravity-ui/uikit";
import type { TableColumnConfig } from "@gravity-ui/uikit";
import {
  getGetMetadataPlaneStatusQueryKey,
  useGetMetadataPlaneStatus,
  useRebuildMetadataPlane,
} from "../api/admin";
import type { MetadataPlaneNode, MetadataPlaneRange, MetadataPlaneStatus } from "../api/model";
import { errText, ErrorAlert, head, KV, Loading, Mono, Panel, Rule } from "./ui";
import { fmtNum } from "../lib/format";
import { PLANE_POLL } from "../lib/poll";

/** The page's "nothing here" mark, matching the disk and object rows above. */
const EMPTY = "\u2014";

/**
 * Range boundaries are positions in the raw key space, and its two ends are the
 * empty string. Rendered as nothing that leaves a blank cell, which reads as
 * missing data rather than as "from the beginning".
 */
function bound(key: string | undefined): string {
  return key ? key : "(start)";
}

/**
 * Why a plane is unusable decides whether anything will fix it on its own, so
 * the cause is said in those terms rather than repeated as a code.
 */
const CAUSE: Record<string, string> = {
  orphaned: "a failure left a range with no copy of its data",
  "never-built": "switched on over existing objects, so a rebuild is owed",
  unspecified: "reason not recorded, so nothing will rebuild it on its own",
};

const RANGES: TableColumnConfig<MetadataPlaneRange>[] = [
  {
    id: "range",
    name: head("range"),
    primary: true,
    template: (r) => (
      <Mono>
        {bound(r.start)} .. {r.end ? r.end : "(end)"}
      </Mono>
    ),
  },
  {
    id: "owner",
    name: head("owner"),
    template: (r) => <Mono>{r.owner}</Mono>,
  },
  {
    id: "replicas",
    name: head("replicas"),
    // Followers can be promoted; learners hold only part of the range and must
    // never be. One undifferentiated list would hide the distinction the whole
    // move design rests on.
    template: (r) => {
      const followers = r.followers ?? [];
      const learners = r.learners ?? [];

      if (followers.length === 0 && learners.length === 0) {
        return (
          <Text
            variant="code-inline-1"
            color="hint"
            title="No replica: losing this owner costs a rebuild rather than a promotion."
          >
            none
          </Text>
        );
      }

      return (
        <Flex gap={1} wrap>
          {followers.map((n) => (
            <Label key={n} size="xs" theme="unknown" title="Follower: kept current by the log, promotable.">
              {n}
            </Label>
          ))}
          {learners.map((n) => (
            <Label
              key={n}
              size="xs"
              theme="warning"
              title="Learner: still being copied into, so it holds only part of this range and must not be promoted."
            >
              {n}
            </Label>
          ))}
        </Flex>
      );
    },
  },
  {
    id: "moving",
    name: head("moving to"),
    template: (r) =>
      r.move_to ? (
        <Mono>{r.move_to}</Mono>
      ) : (
        <Text variant="code-inline-1" color="hint">
          {EMPTY}
        </Text>
      ),
  },
  {
    id: "status",
    name: head("status"),
    align: "end",
    template: (r) =>
      r.status === "held" ? (
        <Label
          theme="danger"
          size="xs"
          title="Owner is gone and the controller is still inside its grace: nobody is serving these keys right now."
        >
          held
        </Label>
      ) : (
        <Label theme="success" size="xs">
          served
        </Label>
      ),
  },
];

const NODES: TableColumnConfig<MetadataPlaneNode>[] = [
  { id: "id", name: head("node"), primary: true, template: (n) => <Mono>{n.id}</Mono> },
  {
    id: "revision",
    name: head("routing by"),
    align: "end",
    // The disagreement is the point. Routing is lazy — a node refreshes when a
    // peer tells it that it is behind — so one taking no traffic for a range
    // that moved can sit on a stale map indefinitely with nothing to notice.
    template: (n) => {
      if (!n.reporting) {
        return (
          <Text
            variant="code-inline-1"
            color="hint"
            title="This node did not answer, so nothing is known about the map it is routing by."
          >
            no answer
          </Text>
        );
      }

      const rev = `r${n.revision ?? 0}`;

      return n.behind ? (
        <Label theme="warning" size="xs" title="Routing by an older map than the control plane holds.">
          {rev}
        </Label>
      ) : (
        <Mono>{rev}</Mono>
      );
    },
  },
  {
    id: "owned",
    name: head("serves"),
    align: "end",
    template: (n) =>
      n.reporting ? (
        <Mono>{fmtNum(n.owned ?? 0)}</Mono>
      ) : (
        <Text variant="code-inline-1" color="hint">
          {EMPTY}
        </Text>
      ),
  },
  {
    id: "replicated",
    name: head("replicates"),
    align: "end",
    template: (n) =>
      n.reporting ? (
        <Mono>{fmtNum(n.replicated ?? 0)}</Mono>
      ) : (
        <Text variant="code-inline-1" color="hint">
          {EMPTY}
        </Text>
      ),
  },
];

/** What is wrong right now, said before the tables that explain it. */
function Headline({ p }: { p: MetadataPlaneStatus }) {
  const ranges = p.ranges ?? [];
  const nodes = p.nodes ?? [];

  const held = ranges.filter((r) => r.status === "held").length;
  const stale = nodes.filter((n) => n.reporting && n.behind).length;
  const silent = nodes.filter((n) => !n.reporting).length;

  return (
    <Flex gap={2} wrap alignItems="center">
      {p.state === "ready" ? (
        <Label theme="success">ready</Label>
      ) : (
        <Label theme="warning">building</Label>
      )}

      {/* A held range is a live partial outage: those keys are answered by
          nobody until the controller's grace expires. It leads. */}
      {held > 0 && (
        <Label
          theme="danger"
          title="Ranges whose owner is gone: nobody is serving those keys right now."
        >
          {`${fmtNum(held)} held`}
        </Label>
      )}

      {stale > 0 && (
        <Label
          theme="warning"
          title="Nodes routing by an older map than the control plane holds."
        >
          {`${fmtNum(stale)} stale`}
        </Label>
      )}

      {silent > 0 && (
        <Label theme="unknown" title="Nodes that did not answer.">
          {`${fmtNum(silent)} silent`}
        </Label>
      )}

      {p.rebuilding && <Label theme="info">rebuilding</Label>}
    </Flex>
  );
}

/**
 * The sharded metadata plane: the partitioning, and what each node believes
 * about it.
 *
 * The metrics answer neither question — they are per node, and the partitioning
 * is a cluster-wide object.
 */
export function MetadataPlane() {
  const toaster = useToaster();
  const qc = useQueryClient();

  const { data, error, isLoading } = useGetMetadataPlaneStatus({
    query: { refetchInterval: PLANE_POLL },
  });

  const rebuild = useRebuildMetadataPlane({
    mutation: {
      onSuccess: () => {
        toaster.add({ name: "plane-rebuild", theme: "success", title: "Rebuild started" });
        void qc.invalidateQueries({ queryKey: getGetMetadataPlaneStatusQueryKey() });
      },
      onError: (e) =>
        toaster.add({
          name: "plane-rebuild",
          theme: "danger",
          title: "Rebuild not started",
          content: errText(e),
        }),
    },
  });

  if (isLoading) return <Loading />;
  if (error) return <ErrorAlert error={error} what="metadata plane" />;

  // A node that does not run the sharded plane has nothing to show, and an
  // empty panel saying so would be one more thing to read past on every cluster
  // that does not use it.
  if (!data || data.state === "disabled") return null;

  const ranges = data.ranges ?? [];
  const nodes = data.nodes ?? [];

  const detail: [ReactNode, ReactNode][] = [
    ["revision", <Mono>{`r${data.revision ?? 0}`}</Mono>],
    [
      "state",
      <Text variant="body-1" color={data.state === "ready" ? undefined : "warning"}>
        {data.state === "ready"
          ? "listings are served from the plane"
          : `listings fall back to walking sidecars: ${
              CAUSE[data.cause ?? "unspecified"] ?? "reason not recorded"
            }`}
      </Text>,
    ],
    ["auto rebuild", <Mono>{data.policy ?? "on_failure"}</Mono>],
  ];

  if (data.error_message) {
    detail.push(["last rebuild", <Text color="danger">{data.error_message}</Text>]);
  }

  return (
    <Flex direction="column" gap={3}>
      <Rule>Metadata plane</Rule>

      <Panel
        title={<Headline p={data} />}
        actions={
          <Button
            view="outlined"
            size="s"
            loading={rebuild.isPending}
            // Offered whatever the state. The plane is rebuilt from the objects
            // themselves, so a rebuild of a ready plane is expensive and never
            // wrong — and refusing would leave an operator who suspects the
            // index with nothing to do about it.
            onClick={() => rebuild.mutate()}
          >
            Rebuild now
          </Button>
        }
      >
        <KV rows={detail} />
      </Panel>

      {ranges.length > 0 && (
        <Panel title="Ranges" sub={`${fmtNum(ranges.length)} in key order`} scroll>
          <Table
            data={ranges}
            columns={RANGES}
            getRowId={(r) => `${r.start} ${r.end}`}
            width="max"
          />
        </Panel>
      )}

      {nodes.length > 0 && (
        <Panel title="Nodes" sub="which map each is routing by" scroll>
          <Table data={nodes} columns={NODES} getRowId="id" width="max" />
        </Panel>
      )}
    </Flex>
  );
}
