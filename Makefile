.PHONY: check

check:
	@test -x "$$(go env GOPATH)/bin/staticcheck" || { \
		echo "staticcheck not found at $$(go env GOPATH)/bin/staticcheck (install: go install honnef.co/go/tools/cmd/staticcheck@2026.1)" >&2; \
		exit 1; \
	}
	@unformatted=$$(gofmt -l .) || exit $$?; \
	if [ -n "$$unformatted" ]; then \
		echo "Files not gofmt-formatted:" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi
	CGO_ENABLED=1 go vet ./...
	CGO_ENABLED=1 go test -race ./...
	CGO_ENABLED=1 go build -o /dev/null ./cmd/codegraph-go
	"$$(go env GOPATH)/bin/staticcheck" ./...
