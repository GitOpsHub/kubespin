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
	@$(MAKE) --no-print-directory install
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

## Spins up a --spot dev cluster on all three clouds in parallel, using the
## cheapest-per-cloud recipe from docs/low-cost-dev-clusters.md#putting-it-together.
## --access public + --authorized-cidrs (pinned to the caller's own IP, so the
## Argo CD install step below can reach each API server without a VPN/bastion)
## rather than the doc's --access private default, since a bare `make spot`
## has no such reachability to assume.
##
## Requires GITHUB_TOKEN (env, for repo creation), GITHUB_ORG, GCP_PROJECT, and
## AZURE_SUBSCRIPTION_ID; cluster IDs/regions are overridable, e.g.:
##   make spot AWS_REGION=us-west-2 AWS_CLUSTER_ID=my-aws-dev
## Requires `kubespin login` to have already authenticated all three clouds,
## and KUBESPIN_REGISTRY_DSN to be set (env/.env), same as any real apply.
AWS_REGION        ?= us-east-1
AWS_CLUSTER_ID     ?= eks-spot-dev
GCP_REGION         ?= us-central1
GCP_CLUSTER_ID     ?= gke-spot-dev
AZURE_REGION       ?= eastus
AZURE_CLUSTER_ID   ?= aks-spot-dev
## Addon set for the spot clusters. `apply` takes --size (small|medium|large);
## the profile flag this used to pass no longer exists on the command.
SPOT_SIZE          ?= small

.PHONY: spot
spot: build
	@org="$$GITHUB_ORG"; \
	proj="$$GCP_PROJECT"; \
	test -n "$$proj" || proj="$$(gcloud config get-value project 2>/dev/null | grep -v '^(unset)$$' || true)"; \
	sub="$$AZURE_SUBSCRIPTION_ID"; \
	test -n "$$sub" || sub="$$(az account show --query id -o tsv 2>/dev/null || true)"; \
	test -n "$$org" || { echo "GITHUB_ORG must be set (GitHub org cluster repos are created in) — add it to .env or export it" >&2; exit 1; }; \
	test -n "$$proj" || { echo "GCP_PROJECT must be set (GCP project hosting the GKE cluster) — add it to .env, export it, or set a default with 'gcloud config set project'" >&2; exit 1; }; \
	test -n "$$sub" || { echo "AZURE_SUBSCRIPTION_ID must be set (Azure subscription hosting the AKS cluster) — add it to .env, export it, or run 'az login'" >&2; exit 1; }; \
	ip="$$(curl -s https://checkip.amazonaws.com)"; \
	test -n "$$ip" || { echo "could not determine this machine's public IP for --authorized-cidrs" >&2; exit 1; }; \
	echo "==> spinning up spot clusters on aws, gcp, azure (authorized for $$ip/32)"; \
	( kubespin apply --provider aws --region $(AWS_REGION) --cluster-id $(AWS_CLUSTER_ID) \
	    --access public --authorized-cidrs "$$ip/32" --spot \
	    --size $(SPOT_SIZE) --github-org "$$org" 2>&1 | sed 's/^/[aws]   /' ) & \
	( kubespin apply --provider gcp --gcp-project "$$proj" --region $(GCP_REGION) --cluster-id $(GCP_CLUSTER_ID) \
	    --access public --authorized-cidrs "$$ip/32" --spot \
	    --size $(SPOT_SIZE) --github-org "$$org" 2>&1 | sed 's/^/[gcp]   /' ) & \
	( kubespin apply --provider azure --azure-subscription "$$sub" --region $(AZURE_REGION) --cluster-id $(AZURE_CLUSTER_ID) \
	    --access public --authorized-cidrs "$$ip/32" --spot \
	    --size $(SPOT_SIZE) --github-org "$$org" 2>&1 | sed 's/^/[azure] /' ) & \
	wait

## Spins up a fully-managed "autopilot" cluster on each cloud that has one —
## EKS Auto Mode on AWS, GKE Autopilot on GCP — in parallel. See
## docs/autopilot-clusters.md. Azure is deliberately absent: AKS's equivalent
## needs an unreleased beta SDK, so --autopilot is unsupported there.
##
## Like `make spot`, this uses --access public + --authorized-cidrs pinned to
## the caller's own IP rather than the doc's --access private default: the
## Argo CD install step connects to each API server from this machine, and a
## bare `make autopilot` has no VPN/bastion reachability to assume.
##
## No --size/--spot/node-pool flags: the provider manages compute itself (those
## flags are rejected alongside --autopilot), and an Autopilot cluster's addon
## set is only Argo CD regardless of size.
##
## Requires GITHUB_TOKEN (for repo creation) plus GITHUB_ORG and GCP_PROJECT
## — .env is loaded automatically (see DOTENV above), and GCP_PROJECT falls
## back to `gcloud config get-value project`.
## Cluster IDs/regions are overridable, e.g.:
##   make autopilot AWS_REGION=us-west-2 AWS_AUTOPILOT_CLUSTER_ID=my-aws-auto
## Requires `kubespin login` to have already authenticated aws and gcp, and
## KUBESPIN_REGISTRY_DSN to be set (env/.env), same as any real apply.
AWS_AUTOPILOT_CLUSTER_ID ?= eks-auto-dev
GCP_AUTOPILOT_CLUSTER_ID ?= gke-autopilot-dev

.PHONY: autopilot
autopilot: build
	@org="$$GITHUB_ORG"; \
	proj="$$GCP_PROJECT"; \
	test -n "$$proj" || proj="$$(gcloud config get-value project 2>/dev/null | grep -v '^(unset)$$' || true)"; \
	test -n "$$org" || { echo "GITHUB_ORG must be set (GitHub org cluster repos are created in) — add it to .env or export it" >&2; exit 1; }; \
	test -n "$$proj" || { echo "GCP_PROJECT must be set (GCP project hosting the GKE Autopilot cluster) — add it to .env, export it, or set a default with 'gcloud config set project'" >&2; exit 1; }; \
	ip="$$(curl -s https://checkip.amazonaws.com)"; \
	test -n "$$ip" || { echo "could not determine this machine's public IP for --authorized-cidrs" >&2; exit 1; }; \
	echo "==> spinning up autopilot clusters on aws, gcp (authorized for $$ip/32); azure has no equivalent"; \
	( kubespin apply --provider aws --region $(AWS_REGION) --cluster-id $(AWS_AUTOPILOT_CLUSTER_ID) \
	    --access public --authorized-cidrs "$$ip/32" --autopilot \
	    --github-org "$$org" 2>&1 | sed 's/^/[aws]   /' ) & \
	( kubespin apply --provider gcp --gcp-project "$$proj" --region $(GCP_REGION) --cluster-id $(GCP_AUTOPILOT_CLUSTER_ID) \
	    --access public --authorized-cidrs "$$ip/32" --autopilot \
	    --github-org "$$org" 2>&1 | sed 's/^/[gcp]   /' ) & \
	wait

## Tears down everything `make spot` and `make autopilot` create. Deletes are
## idempotent — a cluster that was never created, or is already decommissioned,
## is a no-op — so destroying both sets is safe even if you only spun one up.
## Repositories are archived, not deleted; their history is retained.
##
## Prompts once for confirmation. YES=1 skips it (the per-cluster `kubespin
## delete` prompt is always skipped, since five interleaved prompts across
## backgrounded deletes cannot be answered sensibly).
##
##   make destroy                 # both sets, one prompt
##   make destroy-autopilot       # just the EKS Auto Mode + GKE Autopilot pair
##   make destroy-spot YES=1      # just the three spot clusters, unattended
##
## --access public matches what both targets apply with; `kubespin delete`
## requires it to match the cluster's spec.
YES ?=

confirm_destroy = test -n "$(YES)" || { \
	printf '==> about to delete clusters: %s\n    their repositories are archived, not deleted\n    type yes to continue: ' '$(1)'; \
	read ans; test "$$ans" = yes || { echo "aborted" >&2; exit 1; }; }

.PHONY: destroy
destroy:
	@$(call confirm_destroy,$(AWS_AUTOPILOT_CLUSTER_ID) $(GCP_AUTOPILOT_CLUSTER_ID) $(AWS_CLUSTER_ID) $(GCP_CLUSTER_ID) $(AZURE_CLUSTER_ID))
	@$(MAKE) --no-print-directory destroy-autopilot YES=1
	@$(MAKE) --no-print-directory destroy-spot YES=1

.PHONY: destroy-autopilot
destroy-autopilot: build
	@org="$$GITHUB_ORG"; \
	proj="$$GCP_PROJECT"; \
	test -n "$$proj" || proj="$$(gcloud config get-value project 2>/dev/null | grep -v '^(unset)$$' || true)"; \
	test -n "$$org" || { echo "GITHUB_ORG must be set (GitHub org the cluster repos live in) — add it to .env or export it" >&2; exit 1; }; \
	test -n "$$proj" || { echo "GCP_PROJECT must be set (GCP project hosting the GKE cluster) — add it to .env, export it, or set a default with 'gcloud config set project'" >&2; exit 1; }; \
	$(call confirm_destroy,$(AWS_AUTOPILOT_CLUSTER_ID) $(GCP_AUTOPILOT_CLUSTER_ID)); \
	fail="$$(mktemp -d)"; \
	echo "==> deleting autopilot clusters on aws, gcp"; \
	( ( kubespin delete --provider aws --region $(AWS_REGION) --cluster-id $(AWS_AUTOPILOT_CLUSTER_ID) \
	      --access public --github-org "$$org" --yes 2>&1 || touch "$$fail/aws" ) | sed 's/^/[aws]   /' ) & \
	( ( kubespin delete --provider gcp --gcp-project "$$proj" --region $(GCP_REGION) --cluster-id $(GCP_AUTOPILOT_CLUSTER_ID) \
	      --access public --github-org "$$org" --yes 2>&1 || touch "$$fail/gcp" ) | sed 's/^/[gcp]   /' ) & \
	wait; \
	bad="$$(ls "$$fail")"; rm -rf "$$fail"; \
	test -z "$$bad" || { echo "==> delete failed on: $$bad (rerun to resume; delete is idempotent)" >&2; exit 1; }

.PHONY: destroy-spot
destroy-spot: build
	@org="$$GITHUB_ORG"; \
	proj="$$GCP_PROJECT"; \
	test -n "$$proj" || proj="$$(gcloud config get-value project 2>/dev/null | grep -v '^(unset)$$' || true)"; \
	sub="$$AZURE_SUBSCRIPTION_ID"; \
	test -n "$$sub" || sub="$$(az account show --query id -o tsv 2>/dev/null || true)"; \
	test -n "$$org" || { echo "GITHUB_ORG must be set (GitHub org the cluster repos live in) — add it to .env or export it" >&2; exit 1; }; \
	test -n "$$proj" || { echo "GCP_PROJECT must be set (GCP project hosting the GKE cluster) — add it to .env, export it, or set a default with 'gcloud config set project'" >&2; exit 1; }; \
	test -n "$$sub" || { echo "AZURE_SUBSCRIPTION_ID must be set (Azure subscription hosting the AKS cluster) — add it to .env, export it, or run 'az login'" >&2; exit 1; }; \
	$(call confirm_destroy,$(AWS_CLUSTER_ID) $(GCP_CLUSTER_ID) $(AZURE_CLUSTER_ID)); \
	fail="$$(mktemp -d)"; \
	echo "==> deleting spot clusters on aws, gcp, azure"; \
	( ( kubespin delete --provider aws --region $(AWS_REGION) --cluster-id $(AWS_CLUSTER_ID) \
	      --access public --github-org "$$org" --yes 2>&1 || touch "$$fail/aws" ) | sed 's/^/[aws]   /' ) & \
	( ( kubespin delete --provider gcp --gcp-project "$$proj" --region $(GCP_REGION) --cluster-id $(GCP_CLUSTER_ID) \
	      --access public --github-org "$$org" --yes 2>&1 || touch "$$fail/gcp" ) | sed 's/^/[gcp]   /' ) & \
	( ( kubespin delete --provider azure --azure-subscription "$$sub" --region $(AZURE_REGION) --cluster-id $(AZURE_CLUSTER_ID) \
	      --access public --github-org "$$org" --yes 2>&1 || touch "$$fail/azure" ) | sed 's/^/[azure] /' ) & \
	wait; \
	bad="$$(ls "$$fail")"; rm -rf "$$fail"; \
	test -z "$$bad" || { echo "==> delete failed on: $$bad (rerun to resume; delete is idempotent)" >&2; exit 1; }

.PHONY: clean
clean:
	rm -rf bin/
