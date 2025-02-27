export CGO_CFLAGS_ALLOW=-D__BLST_PORTABLE__
export CGO_CFLAGS=-D__BLST_PORTABLE__

SHELL=/usr/bin/env bash

GOVERSION:=$(shell go version | cut -d' ' -f 3 | cut -d. -f 2)
ifeq ($(shell expr $(GOVERSION) \< 13), 1)
$(warning Your Golang version is go 1.$(GOVERSION))
$(error Update Golang to version $(shell grep '^go' go.mod))
endif

CLEAN:=
BINS:=./venus-wallet

git=$(subst -,.,$(shell git describe --always --match=NeVeRmAtCh --dirty 2>/dev/null || git rev-parse --short HEAD 2>/dev/null))

ldflags=-X=github.com/filecoin-project/venus-wallet/version.CurrentCommit='+git$(git)'
ifneq ($(strip $(LDFLAGS)),)
	ldflags+=-extldflags=$(LDFLAGS)
endif

GOFLAGS+=-ldflags="$(ldflags)"

wallet: show-env 
	rm -f venus-wallet
	go build $(GOFLAGS) -o venus-wallet ./cmd/wallet/main.go
	./venus-wallet --version


linux: 	show-env
	rm -f venus-wallet
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC=x86_64-linux-musl-gcc CGO_LDFLAGS="-static" go build $(GOFLAGS) -o venus-wallet ./cmd/wallet/main.go

show-env:
	@echo '_________________build_environment_______________'
	@echo '| CC=$(CC)'
	@echo '| CGO_CFLAGS=$(CGO_CFLAGS)'
	@echo '| git commit=$(git)'
	@echo '-------------------------------------------------'

lint:
	golangci-lint run

test:
	go test -race ./...

clean:
	rm -rf $(CLEAN) $(BINS)
.PHONY: clean

print-%:
	@echo $*=$($*)

TAG:=test
docker:
ifdef DOCKERFILE
	cp $(DOCKERFILE) ./dockerfile
else
	curl -O https://raw.githubusercontent.com/filecoin-project/venus-docs/master/script/docker/dockerfile
endif
	docker build --build-arg https_proxy=$(BUILD_DOCKER_PROXY) --build-arg BUILD_TARGET=venus-wallet -t venus-wallet .
	docker tag venus-wallet filvenus/venus-wallet:$(TAG)

ifdef PRIVATE_REGISTRY
	docker tag venus-wallet $(PRIVATE_REGISTRY)/filvenus/venus-wallet:$(TAG)
endif
.PHONY: docker

docker-push: docker
	docker push $(PRIVATE_REGISTRY)/filvenus/venus-wallet:$(TAG)
