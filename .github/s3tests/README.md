# S3 conformance testing (ceph/s3-tests)

The [`s3tests`](../workflows/s3tests.yml) workflow runs the upstream
[ceph/s3-tests](https://github.com/ceph/s3-tests) suite against a freshly
built server. It is the objective measure of how compatible the server is
with real S3 clients.

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

The suite is pinned to a specific upstream commit (`S3TESTS_REF` in the
workflow) so results are reproducible; bump it deliberately and re-baseline
the list.

Every run is the same: the whole of `test_s3.py` and `test_headers.py`
(about 80 seconds), then `scripts/s3tests check` decides the verdict. It
fails on two things, and the second is the important one:

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
# the s3-tests credentials so the suite exercises SigV4 through boto3. The
# anonymous-access tests pass via canned public-read/-write ACLs.
go build -o fs ./cmd/fs
./fs s3 --config .github/s3tests/server.yaml --addr :8077 --root ./.s3data-ci &

# Set up the suite (pin to the same commit as the workflow).
git clone https://github.com/ceph/s3-tests && cd s3-tests
git checkout <S3TESTS_REF>
python -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt

# Run both gated files and capture per-test results. Failures are expected —
# the checker below decides which of them are allowed.
export S3TEST_CONF=/path/to/.github/s3tests/s3tests.conf
python -m pytest s3tests/functional/test_s3.py -q --tb=line --junit-xml=/tmp/s3.xml
python -m pytest s3tests/functional/test_headers.py -q --tb=line --junit-xml=/tmp/headers.xml
```

Then, from the repository root:

```sh
go run ./scripts/s3tests check \
  --ratchet .github/s3tests/known-failures.txt \
  --report /tmp/s3.xml --report /tmp/headers.xml
```

After an `S3TESTS_REF` bump the whole list is re-baselined at once:

```sh
go run ./scripts/s3tests update \
  --ratchet .github/s3tests/known-failures.txt \
  --report /tmp/s3.xml --report /tmp/headers.xml
```

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
S3TEST_CONF=$PWD/.github/s3tests/cluster/s3tests.conf \
  python -m pytest -q s3tests/functional/test_s3.py
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
S3TEST_CONF=/path/to/.github/s3tests/s3tests.conf \
  python -m pytest -q s3tests/functional/test_s3.py::test_the_one_that_failed
```
