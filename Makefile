# Go runs in Docker unless you have Go 1.27 or newer and pass GO=go
# (`make test GO=go`).

GO_IMAGE ?= golang:1.27.4-bookworm
DOCKER   ?= docker run --rm -v "$(CURDIR)":/src -w /src \
	-v minideck-gocache:/root/.cache -v minideck-gomod:/go/pkg/mod
GO       ?= $(DOCKER) $(GO_IMAGE) go
ifeq ($(GO),go)
RUN      :=
GOFMT    := gofmt
else
RUN      := $(DOCKER) $(GO_IMAGE)
GOFMT    := $(DOCKER) $(GO_IMAGE) gofmt
endif
VERSION  ?= dev
GALLERY  ?= build/screens
HOST     ?= root@miyoo

.PHONY: help test vet fmt lint check build package screens deploy clean

help: ## list the targets
	@awk -F':.*## ' '/^[a-z-]+:.*## /{printf "  %-8s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

test: ## unit tests
	$(GO) test ./...

vet: ## go vet, for this computer and for the device (ARMv7)
	$(GO) vet ./...
	$(RUN) env GOOS=linux GOARCH=arm GOARM=7 go vet ./...

fmt: ## gofmt every Go file
	$(GOFMT) -w cmd

lint: ## staticcheck
	$(GO) run honnef.co/go/tools/cmd/staticcheck@2026.2.2 ./...

check: ## what CI checks
	@test -z "$$($(GOFMT) -l cmd)" || { echo "gofmt needed: make fmt"; exit 1; }
	$(MAKE) --no-print-directory vet test
	for f in app/*.sh scripts/*.sh; do sh -n "$$f" || exit 1; done

build: ## the device binaries in dist/, stamped $(VERSION)
	$(RUN) env VERSION=$(VERSION) sh scripts/build.sh

package: build ## the release zip, app.json and SHA256SUMS in dist/
	./scripts/package.sh $(VERSION)

screens: ## draw the app's screens into $(GALLERY)/
	mkdir -p $(GALLERY)
	$(RUN) env MINIDECK_GALLERY=/src/$(GALLERY) go test -count=1 -run 'TestScreens|TestGallery' ./...

deploy: ## build and copy to $(HOST)
	./scripts/deploy.sh $(HOST)

clean:
	rm -rf build dist
