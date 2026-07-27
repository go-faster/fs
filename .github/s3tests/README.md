# S3 conformance testing

The [`s3tests`](../workflows/s3tests.yml) workflow runs
[go-faster/s3t](https://github.com/go-faster/s3t) — a Go port of
[ceph/s3-tests](https://github.com/ceph/s3-tests), shipped as one binary —
against a freshly built server. It is the objective measure of how compatible
the server is with real S3 clients.

`s3t` replaced the Python suite because the deployment story was the problem,
not the tests: pinning a Python toolchain and `boto3`/`botocore` versions into
every CI job, and a runner that could not be made to report a hang. `s3t`
bounds each test itself (`--timeout`, `--stall-timeout`, `--max-leaked`), runs
the suite concurrently, and enforces the deny-list gate directly — so there is
no separate checker step. The whole run takes seconds rather than minutes.

## Files

- **`s3tests.conf`** — suite configuration: endpoint plus the credentials and
  owner identities (`user_id` / `display_name`) the suite signs with.
- **`server.yaml`** — the server config CI starts, carrying those same
  credentials as admin keys (with matching `user_id` / `display_name`, which
  ACL and listing `<Owner>` elements report). Keep the two in sync.
- **`known-failures.txt`** — the **gate**: node IDs of tests expected to
  fail. Every pull request runs the whole suite and fails if anything outside
  this file fails, or if anything inside it passes. One `pytest` node ID per
  line; `#` starts a comment.
- **`STATUS.md`** — the whole suite mapped into clusters: what passes, what is
  skipped and why, and what fails grouped by root cause. Read it to tell a
  deliberate scope decision from a bug worth fixing.

## How it works

`s3t` is pinned to a specific commit (`S3T_REF` in the workflows) so results
are reproducible; bump it deliberately and re-baseline the list. The commit of
ceph/s3-tests it tracks is recorded in s3t's own `UPSTREAM` file.

`s3t` decides the verdict itself. It fails on two things, and the second is
the important one:

- a test **outside** the list failed — a regression;
- a test **inside** the list passed — the list is stale.

That second rule is what makes the list shrink on its own. An allow-list only
grows: a newly passing test stays invisible until someone runs the suite by
hand and promotes it, and a newly failing test outside the list is never
noticed at all. Here both surface on the pull request that caused them.

The list, not a pass percentage, is the compatibility statement — read
inverted: everything not in it passes.

## Working with the list

**When a change makes a test pass, delete its line in the same change.** CI
will tell you which lines to delete, by name. Do not add lines to make CI
green: a new line is a regression you are choosing to keep, and it needs a
reason beside it.

To see where you stand locally:

```sh
# Build and start the server AUTHENTICATED (as CI does): server.yaml carries
# the suite credentials so it exercises SigV4, and a fixture master key so the
# SSE-S3 tests exercise encryption rather than a NotImplemented.
go build -o fs ./cmd/fs
./fs s3 --config .github/s3tests/server.yaml --addr :8077 --root ./.s3data-ci &

go install github.com/go-faster/s3t/cmd/s3t@latest   # or the pinned S3T_REF

# The gate: fails if anything outside the list fails, or anything inside passes.
s3t run -c .github/s3tests/s3tests.conf \
  --known-failures .github/s3tests/known-failures.txt
```

Narrow it while working on one area:

```sh
s3t run -c .github/s3tests/s3tests.conf -k '^versioning'
s3t list -m versioning        # what a selection covers, without a server
s3t list --node-ids           # the form the known-failures file uses
```

After an `S3T_REF` bump, re-baseline from a run's JSON report:

```sh
s3t run -c .github/s3tests/s3tests.conf --json /tmp/s3t.json
# every failing node id, which is what the list holds
jq -r 'select(.status=="failed") | .node_id' /tmp/s3t.json | sort
```

Keep the grouping comments: `make compat` reads the `# --- Name (n) ---`
headers to build `docs/CONFORMANCE.md`, and the reason beside a group is the
part a node id cannot carry.

A flaky test is the one thing this model handles badly: it fails the gate on
runs where it fails and reports a stale entry on runs where it passes. Fix it
or, if it is upstream's flake, list it with a comment saying so.

## Cluster mode

The [`s3tests-cluster`](../workflows/s3tests-cluster.yml) workflow runs the
same suite against a **clustered** fs — three nodes in three racks over etcd,
writing at quorum across failure domains — because a client must not be able
to tell how many nodes are behind the endpoint. `clusterstore`'s Go conformance
test (`storagetest.Run`) already covers the `fs.Storage` contract; it cannot
see HTTP status codes, headers, SigV4 or real boto3 clients, which is the gap
this closes.

Bring the cluster up locally and point the suite at it:

```sh
./scripts/s3tests-cluster.sh up     # etcd (Docker) + 3 fs nodes, S3 on :8177

# The cluster is held to both lists; s3t takes one, so concatenate them.
cat .github/s3tests/known-failures.txt \
    .github/s3tests/cluster/known-failures.txt > /tmp/cluster-known.txt
s3t run -c .github/s3tests/cluster/s3tests.conf --known-failures /tmp/cluster-known.txt

./scripts/s3tests-cluster.sh down
```

Set `ETCD_ENDPOINT` to reuse an etcd you already have (CI does this with a
service container) instead of letting the script start one.

Files, alongside the single-node ones above:

- **`cluster/node.yaml.tmpl`** — one node's config, rendered per node by the
  script. Carries the same s3-tests credentials as `server.yaml`, so the auth
  surface is identical in both modes.
- **`cluster/s3tests.conf`** — suite config; identical to `s3tests.conf` except
  it points at node 0.
- **`cluster/known-failures.txt`** — tests that pass single-node but **fail in
  cluster mode**. The job honors the top-level list and ratchets this one.

`cluster/known-failures.txt` is a bug list, not a scope statement: unlike the
top-level list, where a line means "unimplemented or out of scope", every line
here is behaviour that already works on `storagefs` and is wrong on the
replicated data plane. The cluster job *ratchets* that file and merely honors
the top-level one — a test that fails on one node and passes on three is not a
cluster bug to fix.

## Reproducing a gating failure locally

Run the one test CI named:

```sh
s3t run -c .github/s3tests/s3tests.conf -k '^the_one_that_failed$'
```
