.PHONY: build test vet fmt lint

build:
	go build -o bin/ciorch ./cmd/ciorch

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint: fmt vet
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint 未安装，跳过（同 cirunner 口径）"
