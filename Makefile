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

# Performance gates from DESIGN.md NFR-3 (throughput ratio, O(1) PUT allocs,
# 4 KiB GET p99). Fast and deterministic; runs in CI.
bench-gate:
	go test ./bench -run NFR3 -v
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
