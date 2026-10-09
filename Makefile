GO        ?= go
GOMOD2NIX ?= gomod2nix

GO_SRC ?= $(shell find . -name '*.go')

build:
	nix build .#

test:
	$(GO) test ./...

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
	$(GO) mod tidy

nix/gomod2nix.toml: go.sum ${GO_SRC}
	$(GOMOD2NIX) generate --dir ${CURDIR} --outdir ${@D}
