# S3 conformance testing (ceph/s3-tests)

The [`s3tests`](../workflows/s3tests.yml) workflow runs the upstream
[ceph/s3-tests](https://github.com/ceph/s3-tests) suite against a freshly
built server. It is the objective measure of how compatible the server is
with real S3 clients.

## Files

- **`s3tests.conf`** — suite configuration (endpoint + placeholder
  credentials). The server runs without auth today, so the credentials are
  arbitrary; boto3 signs with them and the server ignores signatures.
- **`allow.txt`** — the **gating** set: node IDs of tests that are expected
  to pass. Pull requests and pushes to `main` run exactly these and fail if
  any regress. One `pytest` node ID per line; `#` starts a comment.

## How it works

The suite is pinned to a specific upstream commit (`S3TESTS_REF` in the
workflow) so results are reproducible; bump it deliberately and re-baseline
the allow-list.

- **PR / push:** run only `allow.txt`, gate on green.
- **Weekly schedule / manual dispatch:** additionally run the full
  `test_s3.py` for information (never fails the job) and upload a JUnit
  report as an artifact. Use that report to find newly passing tests.

The allow-list, not a pass percentage, is the compatibility statement: it
grows as features land. Tests outside it are either unimplemented features
or intentionally out of scope — both are expected to fail and are simply
excluded from gating.

## Growing the allow-list

After a feature lands, promote the tests it makes pass:

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

# Run the full suite and capture per-test results.
S3TEST_CONF=/path/to/.github/s3tests/s3tests.conf \
  python -m pytest s3tests/functional/test_s3.py -q --tb=no \
  --junit-xml=/tmp/s3.xml
```

Extract the passing node IDs from the JUnit report, confirm they pass
**deterministically** (run twice), and add them to `allow.txt`. Keep the
file sorted so diffs are legible. Never add a test that only passes
intermittently — a flaky entry blocks every unrelated PR.

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
  python -m pytest -q $(sed 's/#.*//' /path/to/gating-set.txt)
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
- **`cluster/known-failures.txt`** — allow-listed tests that pass single-node
  but **fail in cluster mode**. The job gates on `allow.txt` minus this file.

`known-failures.txt` is a bug list, not a scope statement: unlike `allow.txt`,
where an absent test means "unimplemented or out of scope", every line here is
behaviour that already works on `storagefs` and is wrong on the replicated data
plane. It should only ever shrink. The file itself documents each root cause.

## Reproducing a gating failure locally

```sh
S3TEST_CONF=/path/to/.github/s3tests/s3tests.conf \
  python -m pytest $(sed 's/#.*//' /path/to/.github/s3tests/allow.txt) -q
```
