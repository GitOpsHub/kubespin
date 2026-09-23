BINARY      := kubespin
PKG         := github.com/GitOpsHub/kubespin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -X $(PKG)/internal/version.Version=$(VERSION) \
               -X $(PKG)/internal/version.Commit=$(COMMIT) \
               -X $(PKG)/internal/version.BuildDate=$(DATE)

GOLANGCI_VERSION := v2.6.2

## Where `install` puts the binary. ~/.local/bin is already on PATH on macOS
## and writable without sudo, unlike /usr/local/bin. Override for elsewhere:
##   make install INSTALL_DIR=/usr/local/bin
INSTALL_DIR ?= $(HOME)/.local/bin

## Loads .env into every recipe's environment, the same file kubespin itself
## auto-loads (GITHUB_TOKEN, KUBESPIN_REGISTRY_DSN, ...). Values are read
## verbatim, so a '#' or '$' in a password survives; surrounding quotes are
## stripped. A variable already set in the environment or on the command line
## always wins, and because this runs before the defaults below, .env can also
## override things like AWS_REGION. Point it elsewhere with DOTENV=path.
DOTENV      ?= .env
DOTENV_FILE := $(wildcard $(DOTENV))
DOTENV_KEYS := $(if $(DOTENV_FILE),$(shell sed -n 's/^[[:space:]]*\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' $(DOTENV_FILE)))

define read_dotenv
define $(1)
$(subst $$,$$$$,$(shell sed -n 's/^[[:space:]]*$(1)=//p' $(DOTENV_FILE) | tail -1 | sed -e 's/^"\(.*\)"$$/\1/' -e "s/^'\(.*\)'$$/\1/"))
endef
$(1) ?= $$($(1))
endef

$(foreach k,$(DOTENV_KEYS),$(if $(filter undefined,$(origin $(k))),$(eval $(call read_dotenv,$(k)))))
$(if $(DOTENV_KEYS),$(eval export $(DOTENV_KEYS)))

.DEFAULT_GOAL := all

.PHONY: all
all: lint test build

## Also installs onto PATH, so `kubespin ...` works from any directory
## without a ./bin/ prefix.
##
## Skipped when CI is set: a runner has no use for it, and writing outside the
## repo tree is a surprising side effect for a build to have. Use `make build
## INSTALL_DIR=...` to redirect it, or a bare `go build` to avoid it entirely.
.PHONY: build
build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)
ifndef CI
	@$(MAKE) --no-print-directory install VERSION='$(VERSION)'
endif

## Copies the built binary onto PATH. Kept separate from build so it can be
## re-run (or pointed somewhere else) without a rebuild.
.PHONY: install
install:
	@test -x bin/$(BINARY) || { echo "bin/$(BINARY) is not built; run 'make build'" >&2; exit 1; }
	@mkdir -p '$(INSTALL_DIR)'
	@install -m 0755 bin/$(BINARY) '$(INSTALL_DIR)/$(BINARY)'
	@printf '==> installed %s %s to %s\n' '$(BINARY)' '$(VERSION)' '$(INSTALL_DIR)/$(BINARY)'

.PHONY: test
test:
	go test -race -cover ./...

## Checks every chart version the catalog pins against its live repository
## (needs the network, and helm for OCI charts). Run before shipping a
## catalog change: a pin that does not resolve otherwise fails only once
## Argo CD tries it on a real cluster.
.PHONY: check-charts
check-charts:
	KUBESPIN_CHECK_CHARTS=1 go test -count=1 -run TestCatalogCharts_Exist -v ./internal/catalog/

## Integration tests need real cloud credentials and a reachable Postgres
## (KUBESPIN_POSTGRES_TEST_DSN); opt-in only.
.PHONY: integration
integration:
	go test -race -tags=integration ./...

.PHONY: lint
lint:
	golangci-lint run

## Regenerates docs/cli from the command tree. Must be a no-op when current.
.PHONY: docs
docs:
	go run ./internal/tools/docsgen

## Serves the docs site locally (Docusaurus, reading from docs/). Installs
## website/node_modules on first run.
.PHONY: docs-serve
docs-serve:
	cd website && npm install && npm start

## Prepends a dated CHANGELOG.md section for VERSION, built from Conventional
## Commit subjects since the last tag. Mainly for local preview — the release
## workflow (.github/workflows/release.yml) runs the same generator itself on
## every merge to main, so this is not part of a normal release.
##   make changelog VERSION=v1.2.3
.PHONY: changelog
changelog:
	@test -n "$(VERSION)" || { echo "usage: make changelog VERSION=vX.Y.Z" >&2; exit 1; }
	go run ./internal/tools/changeloggen render -version $(VERSION)

.PHONY: fmt
fmt:
	go fmt ./...
	go mod tidy

## Installs developer tooling that isn't vendored through go.mod.
.PHONY: bootstrap
bootstrap:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

## ---------------------------------------------------------------------------
## Dev clusters: spot, autopilot, destroy
##
## Every target below takes the clouds to act on as extra goals; with none
## named, it acts on every cloud it supports:
##   make spot                    # aws, gcp and azure
##   make spot aws                # just the EKS spot cluster
##   make autopilot gcp           # just GKE Autopilot
##   make destroy-spot aws gcp    # tear down the EKS + GKE spot clusters
##   make destroy aws             # every aws cluster from both sets
##
## Each one needs `kubespin login` to have authenticated the selected clouds,
## plus GITHUB_TOKEN, GITHUB_ORG and KUBESPIN_REGISTRY_DSN (.env is loaded
## automatically, see DOTENV above). Selecting gcp also needs GCP_PROJECT,
## which falls back to `gcloud config get-value project`. Selecting azure also
## needs AZURE_SUBSCRIPTION_ID, which falls back to `az account show`.
##
## Clusters use --access public with --authorized-cidrs pinned to the caller's
## own IP, not the --access private default from the docs. The Argo CD install
## step connects to each API server from this machine, and a bare `make` run
## has no VPN or bastion reachability to assume. `kubespin delete` requires
## --access to match, so the destroy targets pass it too.
##
## Cluster IDs and regions can be overridden, e.g.:
##   make spot aws AWS_REGION=us-west-2 AWS_CLUSTER_ID=my-aws-dev
## ---------------------------------------------------------------------------
AWS_REGION               ?= us-east-1
GCP_REGION               ?= us-central1
AZURE_REGION             ?= eastus
AWS_CLUSTER_ID           ?= eks-spot-dev
GCP_CLUSTER_ID           ?= gke-spot-dev
AZURE_CLUSTER_ID         ?= aks-spot-dev
AWS_AUTOPILOT_CLUSTER_ID ?= eks-auto-dev
GCP_AUTOPILOT_CLUSTER_ID ?= gke-autopilot-dev
## Addon set for the spot clusters: --size small|medium|large.
SPOT_SIZE                ?= small
## Set YES=1 to skip the destroy confirmation prompt.
YES                      ?=

## Clouds named as goals. They exist as targets only so that `make spot aws`
## parses; on their own they do nothing.
CLOUDS := $(filter aws gcp azure,$(MAKECMDGOALS))
.PHONY: aws gcp azure
aws gcp azure:
	@:

## Autopilot has no Azure equivalent: AKS Automatic needs a beta SDK that
## kubespin does not depend on, so --autopilot is unsupported there.
SPOT_CLOUDS      := $(or $(CLOUDS),aws gcp azure)
AUTOPILOT_CLOUDS := $(or $(filter aws gcp,$(CLOUDS)),$(if $(CLOUDS),,aws gcp))

## Resolves and validates what the clouds in $(1) need, into $$org, $$proj and
## $$sub.
define prereq_common
org="$$GITHUB_ORG"; \
test -n "$$org" || { echo "GITHUB_ORG must be set (GitHub org the cluster repos live in); add it to .env or export it" >&2; exit 1; };
endef
define prereq_gcp
proj="$$GCP_PROJECT"; \
test -n "$$proj" || proj="$$(gcloud config get-value project 2>/dev/null | grep -v '^(unset)$$' || true)"; \
test -n "$$proj" || { echo "GCP_PROJECT must be set (GCP project hosting the GKE cluster); add it to .env, export it, or run 'gcloud config set project'" >&2; exit 1; };
endef
define prereq_azure
sub="$$AZURE_SUBSCRIPTION_ID"; \
test -n "$$sub" || sub="$$(az account show --query id -o tsv 2>/dev/null || true)"; \
test -n "$$sub" || { echo "AZURE_SUBSCRIPTION_ID must be set (Azure subscription hosting the AKS cluster); add it to .env, export it, or run 'az login'" >&2; exit 1; };
endef
prereqs = $(prereq_common) $(if $(filter gcp,$(1)),$(prereq_gcp)) $(if $(filter azure,$(1)),$(prereq_azure))

define caller_ip
ip="$$(curl -s https://checkip.amazonaws.com)"; \
test -n "$$ip" || { echo "could not determine this machine's public IP for --authorized-cidrs" >&2; exit 1; };
endef

## Per-cloud flags that pick the cluster's location.
where_aws   = --provider aws --region $(AWS_REGION)
where_gcp   = --provider gcp --gcp-project "$$proj" --region $(GCP_REGION)
where_azure = --provider azure --azure-subscription "$$sub" --region $(AZURE_REGION)

spot_id_aws        = $(AWS_CLUSTER_ID)
spot_id_gcp        = $(GCP_CLUSTER_ID)
spot_id_azure      = $(AZURE_CLUSTER_ID)
autopilot_id_aws   = $(AWS_AUTOPILOT_CLUSTER_ID)
autopilot_id_gcp   = $(GCP_AUTOPILOT_CLUSTER_ID)

## The kubespin invocation for each (set, action, cloud).
spot_apply_flags      = --access public --authorized-cidrs "$$ip/32" --spot --size $(SPOT_SIZE) --github-org "$$org"
autopilot_apply_flags = --access public --authorized-cidrs "$$ip/32" --autopilot --github-org "$$org"
delete_flags          = --access public --github-org "$$org" --yes
apply_cmd  = kubespin apply $(where_$(2)) --cluster-id $($(1)_id_$(2)) $($(1)_apply_flags)
delete_cmd = kubespin delete $(where_$(2)) --cluster-id $($(1)_id_$(2)) $(delete_flags)

## Runs $(1)_cmd for set $(2) on every cloud in $(3) in parallel, prefixing
## each line of output with its cloud, and fails listing the clouds that did.
define run_parallel
fail="$$(mktemp -d)"; \
$(foreach c,$(3),( ( $(call $(1)_cmd,$(2),$(c)) 2>&1 || touch "$$fail/$(c)" ) | sed "s/^/$$(printf '%-8s' '[$(c)]')/" ) & ) \
wait; \
bad="$$(ls "$$fail")"; rm -rf "$$fail"; \
test -z "$$bad" || { echo "==> $(1) failed on:" $$bad "(rerun to resume)" >&2; exit 1; }
endef

confirm_destroy = test -n "$(YES)" || { \
	printf '==> about to delete clusters: %s\n    their GitHub repositories are deleted too, irreversibly\n    type yes to continue: ' '$(1)'; \
	read ans; test "$$ans" = yes || { echo "aborted" >&2; exit 1; }; }

## Spins up a --spot dev cluster on each selected cloud, using the
## cheapest-per-cloud recipe from docs/low-cost-dev-clusters.md#putting-it-together.
.PHONY: spot
spot: build
	@$(call prereqs,$(SPOT_CLOUDS)) \
	$(caller_ip) \
	echo "==> spinning up spot clusters on $(SPOT_CLOUDS) (authorized for $$ip/32)"; \
	$(call run_parallel,apply,spot,$(SPOT_CLOUDS))

## Spins up a fully managed cluster on each selected cloud that has one: EKS
## Auto Mode on AWS and GKE Autopilot on GCP (see docs/autopilot-clusters.md).
## It takes no --size or --spot flags because the provider manages compute
## itself, and the addon set is Argo CD alone.
.PHONY: autopilot
autopilot: build
	@test -n "$(AUTOPILOT_CLOUDS)" || { echo "autopilot supports aws and gcp only" >&2; exit 1; }; \
	$(call prereqs,$(AUTOPILOT_CLOUDS)) \
	$(caller_ip) \
	echo "==> spinning up autopilot clusters on $(AUTOPILOT_CLOUDS) (authorized for $$ip/32)"; \
	$(call run_parallel,apply,autopilot,$(AUTOPILOT_CLOUDS))

## Tears down what `make spot` and `make autopilot` created. Each cluster's
## registry record and GitHub repository are deleted with it, so the cluster ID
## is free to reuse, and that history cannot be recovered. A failed delete
## resumes when rerun.
##
## `destroy` asks for confirmation once. The per-cluster `kubespin delete`
## prompt is always skipped, because interleaved prompts from parallel deletes
## cannot be answered sensibly.
.PHONY: destroy
destroy:
	@$(call confirm_destroy,$(strip $(foreach c,$(AUTOPILOT_CLOUDS),$(autopilot_id_$(c))) $(foreach c,$(SPOT_CLOUDS),$(spot_id_$(c)))))
	@$(if $(AUTOPILOT_CLOUDS),$(MAKE) --no-print-directory destroy-autopilot $(CLOUDS) YES=1,:)
	@$(MAKE) --no-print-directory destroy-spot $(CLOUDS) YES=1

.PHONY: destroy-spot
destroy-spot: build
	@$(call prereqs,$(SPOT_CLOUDS)) \
	$(call confirm_destroy,$(foreach c,$(SPOT_CLOUDS),$(spot_id_$(c)))); \
	echo "==> deleting spot clusters on $(SPOT_CLOUDS)"; \
	$(call run_parallel,delete,spot,$(SPOT_CLOUDS))

.PHONY: destroy-autopilot
destroy-autopilot: build
	@test -n "$(AUTOPILOT_CLOUDS)" || { echo "autopilot supports aws and gcp only" >&2; exit 1; }; \
	$(call prereqs,$(AUTOPILOT_CLOUDS)) \
	$(call confirm_destroy,$(foreach c,$(AUTOPILOT_CLOUDS),$(autopilot_id_$(c)))); \
	echo "==> deleting autopilot clusters on $(AUTOPILOT_CLOUDS)"; \
	$(call run_parallel,delete,autopilot,$(AUTOPILOT_CLOUDS))

.PHONY: clean
clean:
	rm -rf bin/
