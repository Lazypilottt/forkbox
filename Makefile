STATICCHECK_VERSION ?= 2026.2.2
GOTESTFLAGS ?=

.PHONY: build agent proto check integration fmt

# Host binaries, plus the static guest agent next to them (templates use it).
build: agent
	go build -o bin/ ./cmd/forkbox ./cmd/forkboxd

# The guest agent: static, for x86_64 Linux guests.
agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/forkbox-agent ./cmd/forkbox-agent

# Regenerate proto/agentpb, proto/forkboxpb and the Python SDK stubs.
# Needs protoc, protoc-gen-go v1.36.6, protoc-gen-go-grpc v1.6.1, and
# grpcio-tools in sdk/python/.venv (see sdk/python/README.md).
proto:
	protoc --proto_path=proto --go_out=proto/agentpb --go_opt=paths=source_relative agent.proto
	protoc --proto_path=proto --go_out=proto/forkboxpb --go_opt=paths=source_relative \
		--go-grpc_out=proto/forkboxpb --go-grpc_opt=paths=source_relative forkbox.proto
	sdk/python/.venv/bin/python -m grpc_tools.protoc --proto_path=proto \
		--python_out=sdk/python/src/forkbox/_proto --pyi_out=sdk/python/src/forkbox/_proto \
		--grpc_python_out=sdk/python/src/forkbox/_proto forkbox.proto
	sed -i.bak 's/^import forkbox_pb2 as/from . import forkbox_pb2 as/' sdk/python/src/forkbox/_proto/forkbox_pb2_grpc.py
	rm -f sdk/python/src/forkbox/_proto/*.bak

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
	go test -tags integration -count=1 -p 1 $(GOTESTFLAGS) -exec 'sudo -n' ./...
