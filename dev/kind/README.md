# Clustered fs on kind

A local, throwaway deployment of clustered fs — three fs nodes across three
failure domains, a three-member etcd, an S3 bucket and a credential — driven by
the two operators that run it in production:

| Component | Managed by |
|---|---|
| fs cluster, buckets, access keys | [fs-operator](https://github.com/go-faster/fs-operator), Helm chart `oci://ghcr.io/go-faster/charts/fs-operator` |
| etcd | [etcd-operator](https://github.com/etcd-io/etcd-operator), from a local checkout |

fs-operator deliberately does not manage etcd: `FSCluster` only accepts
external endpoints. Running etcd under its own operator keeps that boundary
honest instead of papering over it with a hand-rolled StatefulSet.

## Requirements

`kind`, `kubectl`, `helm` and Docker, plus a checkout of
[etcd-io/etcd-operator](https://github.com/etcd-io/etcd-operator). The Makefile
looks for it at `/src/etcd/etcd-operator`; override with
`make ETCD_OPERATOR_DIR=<path>`.

## Up

```sh
make up
```

That is: create the kind cluster, install both operators, bring up etcd, apply
the `FSCluster`, and wait for everything to report Ready. It takes a few
minutes on a cold cache, mostly pulling images, and is idempotent — re-running
it converges rather than starting over.

Then talk to it:

```sh
make port-forward   # blocks: S3 on http://127.0.0.1:8080
make creds          # access-key / secret-key for the "dev" bucket
```

```sh
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_REGION=us-east-1
aws --endpoint-url http://127.0.0.1:8080 s3 cp ./file s3://dev/file
```

`make status` shows the custom resources, pods and claims; `make logs` and
`make logs-fs` follow the operator and the fs nodes.

## Down

```sh
make clean   # drop the fs cluster and its data, keep kind, etcd and the operators
make down    # delete the kind cluster
```

## What the shape is and why

**Three racks of one node, one rack per zone.** `kind.yaml` labels the three
workers `topology.kubernetes.io/zone=zone-{a,b,c}`, and each rack in
`manifests/20-fscluster.yaml` pins to one of them. The operator turns that into
required node affinity, so a rack really is a machine here.

Three failure domains is not arbitrary: the default `rf2.5` scheme places two
full replicas plus parity across three distinct domains, so three is the
smallest topology it is placeable on. `podAntiAffinity: Required` is
satisfiable for the same reason. Drain a worker and the cluster degrades the
way a zone outage would, rather than the way an overcommitted laptop would.

**Everything is pinned.** The chart version (`0.5.0`), the etcd-operator
release (`v0.2.0`) and the etcd version are all fixed in the Makefile and the
manifests. The fs node image is the one exception, and deliberately so: it is
omitted from the `FSCluster`, which defaults it to the fs release that operator
version was validated against.

**Dev-only choices are marked as such** in `_deploy/fs.yml` and the manifests —
plaintext metrics, no cert-manager, no ServiceMonitor, `reclaimPolicy: Delete`
on the disks. Each one has a comment saying what production does instead.

## Layout

```
kind.yaml                    the kind cluster: 3 zoned workers
_deploy/fs.yml               Helm values for the fs-operator chart
manifests/00-namespace.yaml  the fs-dev tenant namespace
manifests/10-etcd.yaml       EtcdCluster (etcd-operator)
manifests/20-fscluster.yaml  FSCluster: 3 racks, 1 disk each
manifests/30-tenancy.yaml    FSBucket + FSAccessKey
```

The operators run in `fs-operator-system` and `etcd-operator-system`; etcd and
the fs cluster share the `fs-dev` namespace, which is the tenancy boundary —
`FSBucket` and `FSAccessKey` may only reference an `FSCluster` in their own
namespace.

## Gotchas

**`make clean` purges the etcd prefix, and has to.** `spec.etcd.cleanupOnDelete`
exists in the CRD but is not implemented in fs-operator 0.5.0 — nothing reads
it. Deleting the `FSCluster` therefore leaves everything under `/fs/fs-dev/fs`
in etcd, including the root S3 credential, sealed with a cluster secret the
next incarnation will not have. fs then logs `Skipping unreadable cluster
credential`, does not re-seed the root credential over the stale entry, and the
operator's own S3 client gets `InvalidAccessKeyId` — the `FSCluster` goes Ready
while every `FSBucket` hangs on `cluster S3 not reachable yet`. The
`etcd-purge` target is what makes `make clean && make up` work; drop it once
the operator implements the field.

**The bucket is `reclaimPolicy: Retain`, not `Delete`.** With `Delete` the
operator refuses to finalize a bucket that still holds objects, which is
correct — but it means `make clean` blocks until you empty the bucket by hand.
Retain just stops managing it; the data goes with the cluster's PVCs, which are
reclaimed anyway.

**etcd comes up one member at a time.** The etcd-operator grows the cluster by
promoting learners, so its StatefulSet is briefly "complete" at one and at two
replicas. `make etcd` waits for `status.readyReplicas == 3` rather than
`rollout status`, which would return on the first of those. The `EtcdCluster`
resource carries no status to wait on in v0.2.0.

**The S3 Service is ClusterIP.** `S3ServiceSpec` has no `nodePort` field, so a
`NodePort` Service would get a random port with nothing stable to map into
kind. Hence `make port-forward`.
