.PHONY: install build vet test

install:
	go mod tidy && go mod vendor
	go install github.com/dollarshaveclub/metahelm

build: vet test
	go build -mod tidy -mod vendor

vet:
	go vet $(shell go list ./... |grep pkg/)

test:
	go test -cover $(shell go list ./... |grep pkg/)
