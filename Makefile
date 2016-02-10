.PHONY: all test test-local install-deps lint fmt vet

REPO_NAME = macvlan-docker-plugin
REPO_OWNER = gopher-net
PKG_NAME = github.com/${REPO_OWNER}/${REPO_NAME}
IMAGE = golang:1.5

all: test

test-local: install-deps fmt lint vet
	@echo "+ $@"
	@godep go test -v ./...

test:
	@docker run -v ${shell pwd}:/go/src/${PKG_NAME} -w /go/src/${PKG_NAME} ${IMAGE} make test-local

install-deps:
	@echo "+ $@"
	@go get -u github.com/golang/lint/golint
	@go get github.com/tools/godep

lint:
	@echo "+ $@"
	@test -z "$$(golint ./... | grep -v Godeps/_workspace/src/ | tee /dev/stderr)"

fmt:
	@echo "+ $@"
	@test -z "$$(gofmt -s -l . | grep -v Godeps/_workspace/src/ | tee /dev/stderr)"

vet:
	@echo "+ $@"
	@go vet ./...

