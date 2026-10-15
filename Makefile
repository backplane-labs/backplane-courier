# backplane-courier — message delivery for Backplane installations.
#
# Conventions (shared with backplane): every tool lives in ./bin, pinned
# below; `git clone` + `make configure` = working environment.

SHELL := /bin/bash
.ONESHELL:
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BIN := $(CURDIR)/bin

GOLANGCI_LINT_VERSION      := v2.11.3
EASYP_VERSION              := v0.16.6
PROTOC_GEN_GO_VERSION      := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2

# easyp finds protoc-gen-es / protoc-gen-ws-es in web/node_modules.
EASYP := PATH="$(CURDIR)/web/node_modules/.bin:$(BIN):$$PATH" "$(BIN)/easyp"

BUILD_PKG := github.com/backplane-labs/backplane/pkg/backplane/build
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0)
COMMIT    ?= $(shell git rev-parse --verify -q HEAD 2>/dev/null || echo unknown)
DATE      ?= $(shell date -u +%FT%TZ)
LDFLAGS    = -X $(BUILD_PKG).Service=courier -X $(BUILD_PKG).Version=$(VERSION) -X $(BUILD_PKG).Commit=$(COMMIT) -X $(BUILD_PKG).Date=$(DATE)

MAX_MAJOR := 1

.PHONY: help
help: ## List all targets with explanations
	@grep -hE '^[a-zA-Z0-9_.-]+:.*## ' $(MAKEFILE_LIST) | \
	  awk -F':.*## ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: configure
configure: ## Install the pinned linter and proto generators into ./bin
	mkdir -p "$(BIN)"
	GOBIN="$(BIN)" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	GOBIN="$(BIN)" go install github.com/easyp-tech/easyp/cmd/easyp@$(EASYP_VERSION)
	GOBIN="$(BIN)" go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN="$(BIN)" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	@echo "✓ configure done — tools in $(BIN)"

.PHONY: configure-web
configure-web: ## Install the console module's dependencies (GitHub Packages: see web/README.md)
	cd web && yarn install --frozen-lockfile

.PHONY: gen
gen: ## Generate the internal API: Go stubs and the module's TypeScript client
	"$(BIN)/easyp" lint -p proto
	$(EASYP) generate
	$(EASYP) --cfg easyp.web.yaml generate

.PHONY: lint-proto
lint-proto: ## Lint the proto definitions
	"$(BIN)/easyp" lint -p proto

.PHONY: lint
lint: ## Lint Go code
	"$(BIN)/golangci-lint" run ./...

.PHONY: fmt
fmt: ## Format Go code
	"$(BIN)/golangci-lint" fmt ./...

.PHONY: test
test: ## Unit tests with the race detector 
	go vet ./...
	go test -race ./...

.PHONY: web
web: ## Build the console module into web/dist (embedded into the binary by build)
	cd "$(CURDIR)/web" && yarn build
	rm -rf "$(CURDIR)/internal/ui/dist"/*
	cp -r "$(CURDIR)/web/dist/plugin/." "$(CURDIR)/internal/ui/dist/"
	rm -rf "$(CURDIR)/internal/ui/dist/.vite"

.PHONY: build
build: ## Build the service into ./bin/courier (make web first for a console module inside)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o "$(BIN)/courier" ./cmd/courier

.PHONY: run
run: build ## Run against the local backplane stack (BACKPLANE_CONFIG_FILE=deployments/dev.yaml)
	BACKPLANE_CONFIG_FILE=deployments/dev.yaml BACKPLANE_CONSUL_ADDR=localhost:8500 BACKPLANE_NATS_URL=nats://localhost:4222 \
	BACKPLANE_INTERNAL_PORT=9460 BACKPLANE_PUBLIC_PORT=8087 BACKPLANE_INTERNAL_SECRET=dev-internal-secret-change-me \
		"$(BIN)/courier"

.PHONY: release
release: ## Create and push a version tag; no local builds or tests
	cd "$$(git rev-parse --show-toplevel)"
	if [ -n "$$(git status --porcelain)" ]; then
	  echo "✗ Working tree is not clean — commit or stash first:"
	  git status --short
	  exit 1
	fi
	if ! upstream="$$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null)"; then
	  echo "✗ Current branch has no upstream — push it first."
	  exit 1
	fi
	git fetch --quiet --prune --tags
	read -r behind ahead <<< "$$(git rev-list --left-right --count "$$upstream...HEAD")"
	if [ "$$ahead" -gt 0 ] || [ "$$behind" -gt 0 ]; then
	  echo "✗ Branch differs from $$upstream ($$ahead ahead, $$behind behind) — synchronize first."
	  exit 1
	fi
	cur="$$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | sed 's/^v//' | sort -t. -k1,1n -k2,2n -k3,3n | tail -1)"
	cur="$${cur:-0.0.0}"
	head="$$(git rev-parse --short HEAD)"
	echo "Latest release: v$$cur    HEAD: $$head"
	echo
	echo "  1) bump version"
	echo "  2) recreate last tag (v$$cur) on HEAD   [force]"
	echo "  3) cancel"
	read -r -p "> " action
	case "$$action" in
	1)
	  IFS=. read -r MA MI PA <<< "$$cur"
	  echo
	  echo "  1) major  -> v$$((MA+1)).0.0"
	  echo "  2) minor  -> v$$MA.$$((MI+1)).0"
	  echo "  3) patch  -> v$$MA.$$MI.$$((PA+1))"
	  read -r -p "> " comp
	  case "$$comp" in
	    1) MA=$$((MA+1)); MI=0; PA=0 ;;
	    2) MI=$$((MI+1)); PA=0 ;;
	    3) PA=$$((PA+1)) ;;
	    *) echo "Aborted."; exit 0 ;;
	  esac
	  if [ "$$MA" -gt "$(MAX_MAJOR)" ]; then
	    echo "✗ v$$MA requires semantic import versioning (/v$$MA in the module path)."
	    exit 1
	  fi
	  new="$$MA.$$MI.$$PA"
	  echo
	  echo "Release v$$new — will create tag v$$new on $$head and push."
	  read -r -p "Type 'yes' to proceed: " ok
	  [ "$$ok" = "yes" ] || { echo "Aborted."; exit 0; }
	  git tag -a "v$$new" -m "v$$new"
	  git push origin "v$$new"
	  echo "✓ Released v$$new."
	  ;;
	2)
	  if ! git tag -l "v$$cur" | grep -q .; then
	    echo "✗ No release tag to recreate."; exit 1
	  fi
	  echo
	  echo "Will DELETE and recreate tag v$$cur on $$head, then force-push."
	  read -r -p "Type 'yes' to proceed: " ok
	  [ "$$ok" = "yes" ] || { echo "Aborted."; exit 0; }
	  git tag -d "v$$cur"
	  git push origin ":refs/tags/v$$cur" 2>/dev/null || true
	  git tag -a "v$$cur" -m "v$$cur"
	  git push origin --force "v$$cur"
	  echo "✓ Recreated v$$cur on $$head."
	  ;;
	*) echo "Cancelled." ;;
	esac
