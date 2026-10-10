GO_SRC ?= $(shell find . -name '*.go')

.PHONY: build test test-all update check lint format fmt tidy

build:
	nix build .#

test:
	go test ./...

# Also runs the dconf adapter tests against a throwaway database.
test-all:
	bash scripts/test.sh

update:
	nix flake update

check lint:
	nix flake check

format fmt:
	nix fmt

tidy: go.sum nix/gomod2nix.toml

go.sum: go.mod ${GO_SRC}
	go mod tidy

nix/gomod2nix.toml: go.sum ${GO_SRC}
	gomod2nix generate --dir ${CURDIR} --outdir ${@D}
