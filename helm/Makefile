# Makefile for Helm Chart OCI operations
# Usage: make help

CHART_NAME := go-faster-fs
CHART_PATH := go-faster-fs
CHART_VERSION := $(shell grep '^version:' $(CHART_PATH)/Chart.yaml | awk '{print $$2}')
REGISTRY ?= ghcr.io/go-faster
PACKAGE_FILE := $(CHART_NAME)-$(CHART_VERSION).tgz

.PHONY: help
help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'

.PHONY: lint
lint: ## Lint the Helm chart
	@echo "==> Linting chart..."
	helm lint $(CHART_PATH)

.PHONY: template
template: ## Test chart template rendering
	@echo "==> Testing template rendering..."
	helm template test $(CHART_PATH) --debug

.PHONY: package
package: lint ## Package the Helm chart
	@echo "==> Packaging chart $(CHART_NAME) version $(CHART_VERSION)..."
	helm package $(CHART_PATH)
	@echo "==> Created: $(PACKAGE_FILE)"

.PHONY: verify
verify: ## Verify the packaged chart
	@echo "==> Verifying package $(PACKAGE_FILE)..."
	@if [ ! -f "$(PACKAGE_FILE)" ]; then \
		echo "Error: $(PACKAGE_FILE) not found. Run 'make package' first."; \
		exit 1; \
	fi
	@echo "Package contents:"
	@tar -tzf $(PACKAGE_FILE) | head -20
	@echo "..."
	@echo "==> Package verified"

.PHONY: push
push: package ## Push chart to OCI registry (requires login)
	@echo "==> Pushing $(PACKAGE_FILE) to $(REGISTRY)..."
	helm push $(PACKAGE_FILE) oci://$(REGISTRY)
	@echo "==> Chart pushed successfully"
	@echo "==> Available at: oci://$(REGISTRY)/$(CHART_NAME):$(CHART_VERSION)"

.PHONY: login-ghcr
login-ghcr: ## Login to GitHub Container Registry (requires GITHUB_TOKEN)
	@if [ -z "$(GITHUB_TOKEN)" ]; then \
		echo "Error: GITHUB_TOKEN not set"; \
		exit 1; \
	fi
	@echo "==> Logging in to ghcr.io..."
	@echo $(GITHUB_TOKEN) | helm registry login ghcr.io -u $(GITHUB_USER) --password-stdin
	@echo "==> Login successful"

.PHONY: login-docker
login-docker: ## Login to Docker Hub (interactive)
	@echo "==> Logging in to Docker Hub..."
	helm registry login registry-1.docker.io

.PHONY: pull
pull: ## Pull chart from OCI registry
	@echo "==> Pulling $(CHART_NAME):$(CHART_VERSION) from $(REGISTRY)..."
	helm pull oci://$(REGISTRY)/$(CHART_NAME) --version $(CHART_VERSION)

.PHONY: install-local
install-local: ## Install chart from local files
	@echo "==> Installing chart from local path..."
	helm install test-fs $(CHART_PATH) \
		--namespace test-fs \
		--create-namespace \
		--dry-run --debug

.PHONY: install-oci
install-oci: ## Install chart from OCI registry
	@echo "==> Installing chart from OCI registry..."
	helm install test-fs oci://$(REGISTRY)/$(CHART_NAME) \
		--version $(CHART_VERSION) \
		--namespace test-fs \
		--create-namespace

.PHONY: show-chart
show-chart: ## Show chart metadata from OCI registry
	helm show chart oci://$(REGISTRY)/$(CHART_NAME) --version $(CHART_VERSION)

.PHONY: show-values
show-values: ## Show chart values from OCI registry
	helm show values oci://$(REGISTRY)/$(CHART_NAME) --version $(CHART_VERSION)

.PHONY: clean
clean: ## Remove packaged charts
	@echo "==> Cleaning up..."
	rm -f $(CHART_NAME)-*.tgz
	@echo "==> Cleanup complete"

.PHONY: version
version: ## Show chart version
	@echo "Chart: $(CHART_NAME)"
	@echo "Version: $(CHART_VERSION)"
	@echo "Registry: $(REGISTRY)"

.PHONY: test
test: lint template ## Run all tests
	@echo "==> All tests passed"

.PHONY: release
release: clean test package push ## Full release workflow (lint, test, package, push)
	@echo "==> Release complete!"
	@echo "==> Chart available at: oci://$(REGISTRY)/$(CHART_NAME):$(CHART_VERSION)"

.PHONY: all
all: test package ## Default target: test and package

.DEFAULT_GOAL := help

