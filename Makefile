VERSION ?= 0.2.0
COMMIT ?= unknown
BUILD_TIME ?= unknown
LDFLAGS = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)

.PHONY: test race vet build clean

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

build:
	mkdir -p outputs
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o outputs/mihomo-node-manager ./cmd/mihomo-node-manager

clean:
	rm -f outputs/mihomo-node-manager outputs/mihomo-node-manager-deploy.tar.gz
