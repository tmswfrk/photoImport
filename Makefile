all: build lint

build:
	@go build -o photo_import main.go
	$(info Built as $(shell pwd)/photo_import)

lint:
ifneq ("$(wildcard $(shell which golangci-lint))","")
	@golangci-lint run
else
	$(info golangci-lint not installed or not available in PATH)
endif
