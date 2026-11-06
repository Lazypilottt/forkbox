STATICCHECK_VERSION ?= 2026.2.2
GOTESTFLAGS ?=

.PHONY: build agent proto check integration fmt

# Host binaries, plus the static guest agent next to them (templates use it).
build: agent
	go build -o bin/ ./cmd/forkbox ./cmd/forkboxd

# The guest agent: static, for x86_64 Linux guests.
agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/forkbox-agent ./cmd/forkbox-agent

# Regenerate proto/agentpb (needs protoc and protoc-gen-go v1.36.6).
proto:
	protoc --proto_path=proto --go_out=proto/agentpb --go_opt=paths=source_relative agent.proto

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
integration: agent
	go test -tags integration -count=1 $(GOTESTFLAGS) -exec 'sudo -n' ./...
