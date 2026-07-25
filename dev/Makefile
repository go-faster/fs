# Clustered fs on kind. See README.md.
#
# Targets are ordered the way you use them: `make up` from nothing to a Ready
# cluster, `make port-forward` + `make creds` to talk to it, `make down` to
# throw it away.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

KIND ?= kind
KUBECTL ?= kubectl
HELM ?= helm

# The kind cluster from kind.yaml. kubectl contexts are named kind-<name>.
CLUSTER ?= fs-dev
CONTEXT := kind-$(CLUSTER)
KUBECTL_CTX := $(KUBECTL) --context $(CONTEXT)

# Namespaces: one per operator, one for the tenant (etcd + fs cluster).
OPERATOR_NS ?= fs-operator-system
ETCD_OPERATOR_NS ?= etcd-operator-system
NS ?= fs-dev

# The fs-operator chart. Pinned: an unpinned dev setup drifts from what anyone
# else reproduces.
CHART ?= oci://ghcr.io/go-faster/charts/fs-operator
CHART_VERSION ?= 0.5.0
VALUES ?= _deploy/fs.yml

# etcd is run by the official etcd-operator, installed from its checkout's
# release manifest. Override ETCD_OPERATOR_DIR if yours lives elsewhere.
ETCD_OPERATOR_DIR ?= /src/etcd/etcd-operator
ETCD_OPERATOR_VERSION ?= v0.2.0
ETCD_OPERATOR_MANIFEST := $(ETCD_OPERATOR_DIR)/dist/install-$(ETCD_OPERATOR_VERSION).yaml

# Must match spec.size in manifests/10-etcd.yaml.
ETCD_SIZE ?= 3

# The FSCluster of manifests/20-fscluster.yaml and its S3 Service (named after
# the cluster).
FSCLUSTER ?= fs
LOCAL_PORT ?= 8080

# Where the cluster keeps its state in etcd. This is the operator's default
# (/fs/<namespace>/<name>) — spec.etcd.prefix is left unset and is immutable.
ETCD_PREFIX ?= /fs/$(NS)/$(FSCLUSTER)

# Bounds every wait; a rollout that has not converged by now is a bug, not
# slowness.
TIMEOUT ?= 10m

.PHONY: help
help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: up
up: cluster operator etcd cluster-apply ## Everything: kind cluster -> operators -> etcd -> fs -> bucket + key.
	@echo
	@echo "fs is Ready in namespace $(NS)."
	@$(MAKE) --no-print-directory creds
	@echo
	@echo "Run 'make port-forward' and point an S3 client at http://127.0.0.1:$(LOCAL_PORT)."

.PHONY: cluster
cluster: ## Create the kind cluster (idempotent).
	@if $(KIND) get clusters 2>/dev/null | grep -qx '$(CLUSTER)'; then \
		echo "kind cluster '$(CLUSTER)' already exists"; \
	else \
		$(KIND) create cluster --config kind.yaml; \
	fi

.PHONY: operator
operator: ## Install/upgrade fs-operator from the OCI chart.
	$(HELM) --kube-context $(CONTEXT) upgrade --install fs-operator $(CHART) \
		--values $(VALUES) \
		--version $(CHART_VERSION) \
		--namespace $(OPERATOR_NS) --create-namespace \
		--wait --timeout $(TIMEOUT)

# The two waits below are both deliberate. The etcd-operator creates the
# StatefulSet asynchronously, so the object has to exist before anything can
# wait on it; and it then grows the cluster one member at a time, so the
# StatefulSet is briefly "complete" at one and at two replicas — `rollout
# status` would return on the first of those. Waiting for readyReplicas to
# reach ETCD_SIZE is the quorum fs actually needs. The EtcdCluster resource
# itself carries no status to wait on in etcd-operator v0.2.0.
.PHONY: etcd
etcd: etcd-operator ## Create the three-member EtcdCluster and wait for it.
	$(KUBECTL_CTX) apply -f manifests/00-namespace.yaml
	$(KUBECTL_CTX) apply -f manifests/10-etcd.yaml
	$(KUBECTL_CTX) -n $(NS) wait --for=create statefulset/etcd --timeout=2m
	$(KUBECTL_CTX) -n $(NS) wait --for=jsonpath='{.status.readyReplicas}'=$(ETCD_SIZE) \
		statefulset/etcd --timeout=$(TIMEOUT)

.PHONY: etcd-operator
etcd-operator: ## Install the etcd-operator from $(ETCD_OPERATOR_DIR).
	@test -f $(ETCD_OPERATOR_MANIFEST) || { \
		echo "missing $(ETCD_OPERATOR_MANIFEST)"; \
		echo "clone etcd-io/etcd-operator or set ETCD_OPERATOR_DIR=<path>"; \
		exit 1; \
	}
	$(KUBECTL_CTX) apply -f $(ETCD_OPERATOR_MANIFEST)
	$(KUBECTL_CTX) -n $(ETCD_OPERATOR_NS) rollout status \
		deployment/etcd-operator-controller-manager --timeout=$(TIMEOUT)

.PHONY: cluster-apply
cluster-apply: ## Apply the FSCluster, bucket and access key, then wait for Ready.
	$(KUBECTL_CTX) apply -f manifests/20-fscluster.yaml
	$(KUBECTL_CTX) -n $(NS) wait --for=condition=Ready fscluster/$(FSCLUSTER) --timeout=$(TIMEOUT)
	$(KUBECTL_CTX) apply -f manifests/30-tenancy.yaml
	$(KUBECTL_CTX) -n $(NS) wait --for=condition=Ready fsbucket/dev --timeout=2m
	$(KUBECTL_CTX) -n $(NS) wait --for=condition=Ready fsaccesskey/dev --timeout=2m

.PHONY: status
status: ## Show what the cluster is doing.
	$(KUBECTL_CTX) -n $(NS) get fscluster,fsbucket,fsaccesskey
	@echo
	$(KUBECTL_CTX) -n $(NS) get pods -o wide
	@echo
	$(KUBECTL_CTX) -n $(NS) get pvc

.PHONY: creds
creds: ## Print the generated S3 credential.
	@$(KUBECTL_CTX) -n $(NS) get secret dev-credentials \
		-o go-template='{{range $$k, $$v := .data}}{{$$k}}={{$$v | base64decode}}{{"\n"}}{{end}}'

.PHONY: port-forward
port-forward: ## Forward the S3 endpoint to 127.0.0.1:$(LOCAL_PORT) (blocks).
	$(KUBECTL_CTX) -n $(NS) port-forward svc/$(FSCLUSTER) $(LOCAL_PORT):8080

.PHONY: logs
logs: ## Follow the fs-operator controller logs.
	$(KUBECTL_CTX) -n $(OPERATOR_NS) logs -l control-plane=controller-manager -f --tail=100

.PHONY: logs-fs
logs-fs: ## Follow the logs of every fs node.
	$(KUBECTL_CTX) -n $(NS) logs -l fs.go-faster.org/cluster=$(FSCLUSTER) -f --tail=100 --max-log-requests=16

.PHONY: clean
clean: ## Delete the fs cluster and its tenancy, keep kind and the operators.
	-$(KUBECTL_CTX) delete -f manifests/30-tenancy.yaml --ignore-not-found
	-$(KUBECTL_CTX) delete -f manifests/20-fscluster.yaml --ignore-not-found --wait --timeout=$(TIMEOUT)
	$(MAKE) --no-print-directory etcd-purge

# fs-operator 0.5.0 declares spec.etcd.cleanupOnDelete but does not implement
# it — nothing reads the field. Deleting the FSCluster therefore leaves
# everything under ETCD_PREFIX behind, including the root S3 credential, sealed
# with a cluster secret the next incarnation will not have. fs then logs
# "Skipping unreadable cluster credential", declines to re-seed the root
# credential over the stale entry, and the operator's own S3 client gets
# InvalidAccessKeyId: the FSCluster goes Ready while every FSBucket hangs on
# "cluster S3 not reachable yet". Purging the prefix is what makes
# `make clean && make up` work. Drop this target once the operator implements
# the field.
.PHONY: etcd-purge
etcd-purge: ## Delete the fs cluster's keys from etcd. Part of `clean`.
	@if $(KUBECTL_CTX) -n $(NS) get pod etcd-0 >/dev/null 2>&1; then \
		$(KUBECTL_CTX) -n $(NS) exec etcd-0 -- etcdctl del --prefix $(ETCD_PREFIX); \
	else \
		echo "etcd-0 not running; nothing to purge under $(ETCD_PREFIX)"; \
	fi

.PHONY: down
down: ## Delete the whole kind cluster.
	$(KIND) delete cluster --name $(CLUSTER)
