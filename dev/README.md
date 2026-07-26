# Local deployments

Three ways to run fs on one machine, for three different questions.

| | [`demo/`](demo/) | [`kind/`](kind/) | [`observability/`](observability/) |
|---|---|---|---|
| Runs | a three-node cluster under load | a three-node cluster on Kubernetes | one node with a telemetry stack around it |
| On | Docker Compose | kind | Docker Compose |
| Answers | what does the cluster do under load, and what does the dashboard show | does the operator reconcile this correctly | what do the traces and metrics look like |
| Needs | Docker | kind, kubectl, helm, an etcd-operator checkout | Docker |

**`demo/`** is the one to reach for first: etcd, three nodes, the admin
dashboard and a load generator writing across several buckets at sizes from a
few KiB to tens of MiB. Stop a node and watch the cluster carry on.

**`kind/`** deploys the same cluster the way production does, through
fs-operator and etcd-operator, so it is where operator and Kubernetes questions
get answered.

**`observability/`** runs a single node under Grafana, Tempo, Prometheus and
Alloy — and has Tempo store its traces *in* that node, so fs is both the
subject and the backing store.
