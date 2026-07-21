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
