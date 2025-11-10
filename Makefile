GOHOSTOS:=$(shell go env GOHOSTOS)
GOPATH:=$(shell go env GOPATH)
VERSION=$(shell git describe --tags --always --dirty)
PACKAGES=$(shell go list ./... | grep -v /vendor/)
CURRENTTIME=$(shell date +"%Y%m%d%H%M%S")

REMOTE_DIR=/root/hub-download/dingospeed

ifeq ($(GOHOSTOS), windows)
	#the `find.exe` is different from `find` in bash/shell.
	#to see https://docs.microsoft.com/en-us/windows-server/administration/windows-commands/find.
	#changed to use git-bash.exe to run find cli or other cli friendly, caused of every developer has a Git.
	#Git_Bash= $(subst cmd\,bin\bash.exe,$(dir $(shell where git)))
	Git_Bash=$(shell which bash)
	PROTO_FILES=$(shell $(Git_Bash) -c "find . -name *.proto")
	TEST_DIRS=$(shell $(Git_Bash) -c "find . -name '*_test.go' | awk -F '/[^/]*$$' '{print $$1}' | sort -u")
	GO_FILES=$(shell $(Git_Bash) -c "find . -name '*.go' -type f -not -path './vendor/*'")
else
	PROTO_FILES=$(shell find . -name *.proto)
	TEST_DIRS=$(shell find . -name '*_test.go' | awk -F '/[^/]*$$' '{print $$1}' | sort -u)
    GO_FILES=$(shell find . -name '*.go' -type f -not -path './vendor/*')
endif

.PHONY: init
# init env
init:
	go install github.com/google/wire/cmd/wire@latest

.PHONY: wire
# wire
wire:
	cd cmd/ && wire gen ./...

.PHONY: generate
# generate
generate:
	go mod tidy
	go get github.com/google/wire/cmd/wire@latest
	go generate ./...

.PHONY: proto
# generate proto
proto:
	protoc --proto_path=./pkg/proto \
 	       --go_out=paths=source_relative:./pkg/proto \
 	       --go-grpc_out=paths=source_relative:./pkg/proto \
	       $(PROTO_FILES)

.PHONY: test
# test
test:
	@go clean -testcache && go test -cover -v ${TEST_DIRS} -gcflags="all=-N -l"

.PHONY: vet
# vet
vet:
	@go vet --unsafeptr=false $(PACKAGES)

.PHONY: build
# build
build:
	mkdir -p bin/ && go build -ldflags "-s -w -X main.Version=$(VERSION)" -o ./bin/dingospeed dingospeed/cmd

.PHONY: macbuild
macbuild:
	mkdir -p bin/ && CGO_ENABLED=0 GOOS=linux GOARCH=amd64  go build -ldflags "-s -w -X main.Version=$(VERSION)" -o ./bin/dingospeed dingospeed/cmd

.PHONY: repairbuild
repairbuild:
	mkdir -p bin/ && CGO_ENABLED=0 GOOS=linux GOARCH=amd64  go build -ldflags "-s -w -X main.Version=$(VERSION)" -o ./bin/repair dingospeed/repair

.PHONY: repairScpDev
repairScpDev:
	scp bin/repair root@172.30.14.123:/root/hub-download/dingospeed


.PHONY: scpDev
scpDev:
	scp bin/dingospeed root@172.30.14.123:/root/hub-download/dingospeed

.PHONY: scpTest
scpTest:
	scp bin/dingospeed root@10.220.70.124:/root/hub-download/dingospeed

# go install github.com/superproj/addlicense@latest
.PHONY: license
license:
	addlicense -v -f LICENSE cmd pkg internal

.PHONY: docker
docker:
	make macbuild;
	docker buildx build --platform linux/amd64 -f docker/Dockerfile-alpine-simple -t harbor.zetyun.cn/dingofs/dingospeed:$(CURRENTTIME) .
	docker push harbor.zetyun.cn/dingofs/dingospeed:$(CURRENTTIME)

.PHONY: all
# generate all
all:
	make init;
	make generate;
	#make proto;
	make vet;
	#make test;
	make build

# show help
help:
	@echo ''
	@echo 'Usage:'
	@echo ' make [target]'
	@echo ''
	@echo 'Targets:'
	@awk '/^[a-zA-Z\-\_0-9]+:/ { \
	helpMessage = match(lastLine, /^# (.*)/); \
		if (helpMessage) { \
			helpCommand = substr($$1, 0, index($$1, ":")); \
			helpMessage = substr(lastLine, RSTART + 2, RLENGTH); \
			printf "\033[36m%-22s\033[0m %s\n", helpCommand,helpMessage; \
		} \
	} \
	{ lastLine = $$0 }' $(MAKEFILE_LIST)

.DEFAULT_GOAL := help