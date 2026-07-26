test:
	go test --timeout 10m -race ./...
.PHONY: test

coverage:
	go test -race -v -coverpkg=./... -coverprofile=profile.out ./...
	go tool cover -func profile.out
.PHONY: coverage

test_fast:
	go test ./...
.PHONY: test_fast

# Actively fuzz the untrusted-input wire parsers (SigV4, aws-chunked framing,
# XML bodies). FUZZTIME overrides the per-target budget, e.g. FUZZTIME=5m.
fuzz:
	./scripts/fuzz.sh
.PHONY: fuzz

# Check that fuzz.sh still fails on real findings and only retries a search
# stopped by its own deadline. Seconds, no fuzzing — it replays recorded output.
fuzz_selftest:
	./scripts/fuzz_selftest.sh
.PHONY: fuzz_selftest

# Performance gates from DESIGN.md NFR-3 (throughput ratio, O(1) PUT allocs,
# 4 KiB GET p99). FS_PERF_GATES enables the wall-clock gates (throughput,
# latency); the deterministic allocation gate runs regardless.
bench-gate:
	FS_PERF_GATES=1 go test ./bench -run NFR3 -v
.PHONY: bench-gate

# Full benchmark run for benchstat tracking: ns/op, MB/s, allocs/op. Pipe to a
# file and compare across commits with `go tool benchstat old.txt new.txt`.
bench:
	go test ./bench -run '^$$' -bench . -benchmem -count 6
.PHONY: bench

tidy:
	go mod tidy
.PHONY: tidy

generate:
	go generate ./...
.PHONY: generate

# Regenerate docs/CONFORMANCE.md from the s3-tests allow-list.
compat:
	go run ./scripts/gencompat
.PHONY: compat

# Drive a live server with the real S3 CLIs (aws-cli, mc, s3cmd, rclone).
cli-smoke:
	./scripts/cli-smoke.sh
.PHONY: cli-smoke

# Bring up / tear down the 3-node cluster the ceph/s3-tests conformance suite
# runs against (.github/workflows/s3tests-cluster.yml). Needs Docker for etcd
# unless ETCD_ENDPOINT is set. See .github/s3tests/README.md for running the
# suite against it.
s3tests-cluster-up:
	./scripts/s3tests-cluster.sh up
.PHONY: s3tests-cluster-up

s3tests-cluster-down:
	./scripts/s3tests-cluster.sh down
.PHONY: s3tests-cluster-down
