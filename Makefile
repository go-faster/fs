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
