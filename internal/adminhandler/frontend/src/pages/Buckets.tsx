import { Flex, Table, Text } from "@gravity-ui/uikit";
import type { TableColumnConfig } from "@gravity-ui/uikit";
import { useGetBucketUsage } from "../api/admin";
import type { BucketUsage } from "../api/model";
import { ApiError } from "../lib/fetcher";
import { ErrorAlert, head, KV, Loading, Mono, Panel, Rule } from "../components/ui";
import { fmtAge, fmtBytes, fmtNum } from "../lib/format";
import { USAGE_POLL } from "../lib/poll";

const COLUMNS: TableColumnConfig<BucketUsage>[] = [
  { id: "bucket", name: head("bucket"), primary: true },
  {
    id: "objects",
    name: head("objects"),
    align: "end",
    template: (b) => <Mono>{fmtNum(b.objects)}</Mono>,
  },
  {
    id: "bytes",
    name: head("size"),
    align: "end",
    template: (b) => <Mono>{fmtBytes(b.bytes)}</Mono>,
  },
  {
    id: "updated",
    name: head("changed"),
    align: "end",
    template: (b) => (
      <Text
        variant="code-inline-1"
        color="secondary"
        title="When an incremental write or delete last moved these counters."
      >
        {fmtAge(b.updated)}
      </Text>
    ),
  },
  {
    id: "counted",
    name: head("recounted"),
    align: "end",
    // A total that has never been recounted has only ever been maintained
    // incrementally, so nothing has checked it against the objects themselves.
    template: (b) => (
      <Text
        variant="code-inline-1"
        color={b.counted ? undefined : "hint"}
        title={
          b.counted
            ? "When a full recount last verified this total against the objects themselves."
            : "No recount has verified this total yet; it has only ever been maintained incrementally."
        }
      >
        {fmtAge(b.counted)}
      </Text>
    ),
  },
];

export default function Buckets() {
  const usage = useGetBucketUsage({ query: { refetchInterval: USAGE_POLL, retry: false } });

  // The usage index is a cluster facility; a single-backend instance keeps no
  // such counters, and the endpoint says so with a 501 rather than an empty list.
  const err = usage.error as unknown as ApiError | null;
  if (err && err.status === 501) {
    return (
      <Flex direction="column" gap={3}>
        <Rule>Usage</Rule>
        <Panel title="Not accounted here" sub="single-backend instance">
          <Text variant="body-1" color="secondary">
            Per-bucket usage comes from the cluster's durable usage index. This instance serves a
            single filesystem backend and keeps no such counters, so there is nothing to report.
          </Text>
        </Panel>
      </Flex>
    );
  }

  const buckets = usage.data?.buckets ?? [];

  return (
    <Flex direction="column" gap={3}>
      <Rule>Usage</Rule>
      <Panel
        title="Buckets"
        sub={usage.data ? `${buckets.length} accounted` : undefined}
        scroll={buckets.length > 0}
      >
        {usage.isLoading && <Loading />}
        {usage.error && <ErrorAlert error={usage.error} what="bucket usage" />}
        {usage.data && buckets.length === 0 && (
          <Text variant="body-1" color="secondary">
            No bucket has been accounted for yet. Totals appear once objects are written, or after
            the first cluster-wide recount.
          </Text>
        )}
        {buckets.length > 0 && (
          <Table data={buckets} columns={COLUMNS} getRowId="bucket" width="max" />
        )}
      </Panel>

      {buckets.length > 0 && (
        <Panel title="Total" sub="logical — what clients stored">
          <Flex direction="column" gap={3}>
            <KV
              rows={[
                ["objects", <Mono>{fmtNum(usage.data?.objects)}</Mono>],
                ["size", <Mono>{fmtBytes(usage.data?.bytes)}</Mono>],
              ]}
            />
            {/* Say it plainly, so an operator does not read this as disk usage:
                replication and erasure coding multiply it on the way down. */}
            <Text variant="body-1" color="secondary">
              What the cluster holds on disk is this multiplied by each bucket's replication
              scheme.
            </Text>
          </Flex>
        </Panel>
      )}
    </Flex>
  );
}
