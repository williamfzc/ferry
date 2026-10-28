.PHONY: check test cover
check: test
	@test -z "$$(gofmt -l session)"

test:
	go vet ./...
	go test -race ./...

cover:
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
