.PHONY: check

check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "Files not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	CGO_ENABLED=1 go vet ./...
	CGO_ENABLED=1 go test -race ./...
	CGO_ENABLED=1 go build -o /dev/null ./cmd/codegraph-go
	"$$(go env GOPATH)/bin/staticcheck" ./...
