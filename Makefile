STATICCHECK_VERSION ?= 2026.2.2

.PHONY: build check integration fmt

build:
	go build -o bin/ ./cmd/...

fmt:
	gofmt -w .

# Runs anywhere.
check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...
	go vet -tags integration ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
	go test ./...

# On the KVM host. Test binaries run as root (jailer, cgroups, namespaces).
integration:
	go test -tags integration -count=1 -exec 'sudo -n' ./...
