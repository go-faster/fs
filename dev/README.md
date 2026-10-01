# Local deployments

**[`observability/`](observability/)** runs a single node under Grafana, Tempo,
Prometheus and Alloy on Docker Compose — and has Tempo store its traces *in*
that node, so fs is both the subject and the backing store. Reach for it to see
what the traces and metrics look like.
