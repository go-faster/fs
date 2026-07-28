-- objects holds one row per object for the whole cluster. That is the entire
-- point of cluster scope: a listing page is a range scan here rather than a
-- k-way merge of one scan per node.
--
-- COLLATE "C" is load-bearing, not a micro-optimization. S3 lists keys in UTF-8
-- binary order, and so does every other ordered surface in this codebase. Under
-- a language collation 'Z' sorts after 'a' and punctuation is secondary, so a
-- listing would come back in an order the paging cursor is not comparing in --
-- which skips and repeats keys rather than merely looking odd.
CREATE TABLE IF NOT EXISTS "objects" (
    "bucket"      TEXT        COLLATE "C" NOT NULL,
    "key"         TEXT        COLLATE "C" NOT NULL,
    "size"        BIGINT      NOT NULL,
    "etag"        TEXT        NOT NULL DEFAULT '',
    "modified"    TIMESTAMPTZ NOT NULL,
    "seq"         BIGINT      NOT NULL DEFAULT 0,
    "generation"  TEXT        NOT NULL DEFAULT '',
    "disk"        TEXT        NOT NULL DEFAULT '',
    "owner_id"    TEXT        NOT NULL DEFAULT '',
    "owner_name"  TEXT        NOT NULL DEFAULT '',
    -- NULL means never verified, which is how a scrub sweep finds what to do
    -- first. A zero timestamp would sort as "checked long ago" instead, and be
    -- picked up last by exactly the sweep that should reach it first.
    "verified_at" TIMESTAMPTZ,
    PRIMARY KEY ("bucket", "key")
);

-- The primary key's btree is ("bucket", "key") in C collation, so a bucket's
-- objects are already contiguous and in listing order. Scan needs no second
-- index, and adding one would only slow ingest.

-- bucket_usage is a maintained aggregate, not a recount. It moves in the same
-- transaction as the entry that displaces it, so a bucket's totals can never
-- disagree with the rows behind them.
CREATE TABLE IF NOT EXISTS "bucket_usage" (
    "bucket"  TEXT   COLLATE "C" PRIMARY KEY,
    "objects" BIGINT NOT NULL DEFAULT 0,
    "bytes"   BIGINT NOT NULL DEFAULT 0
);

-- store_meta is a single row: whether the store can be trusted. The boolean
-- primary key with a CHECK is how PostgreSQL spells "exactly one row".
CREATE TABLE IF NOT EXISTS "store_meta" (
    "id"    BOOLEAN  PRIMARY KEY DEFAULT TRUE CHECK ("id"),
    "state" SMALLINT NOT NULL DEFAULT 0
);

INSERT INTO "store_meta" ("id", "state") VALUES (TRUE, 0)
ON CONFLICT ("id") DO NOTHING;
