/*
 * How often each resource is re-read.
 *
 * Every call site for a given resource must pass the same interval: react-query
 * dedupes by query key, so one shared value means one request per interval no
 * matter how many components are watching. Two different intervals on the same
 * key would schedule two independent polls.
 */

/** Build and uptime barely move; this is a liveness check as much as a read. */
export const INFO_POLL = 10_000;

/** Placement, capacity and the repair queue are what an operator watches move. */
export const CLUSTER_POLL = 5_000;

/** Usage comes from the durable index and is updated by a background recount. */
export const USAGE_POLL = 30_000;

/** Schema state only changes during an upgrade. */
export const MIGRATION_POLL = 10_000;
