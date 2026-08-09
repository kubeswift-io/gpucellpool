# gpucellpool — build, generate, test.

IMG ?= ghcr.io/kubeswift-io/gpucellpool/manager:latest
CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

CONTROLLER_TOOLS_VERSION ?= v0.17.2
GOLANGCI_LINT_VERSION ?= v2.12.2
ENVTEST_VERSION ?= release-0.23
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRDs and RBAC into config/, and sync the chart copy.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." \
		output:crd:artifacts:config=config/crd/bases
	# helm upgrade never updates files in crds/, so a stale copy here would install
	# an old schema on a fresh cluster and silently drop fields. Copying it as part
	# of generation means it cannot drift; CI's diff then guards a machine step
	# rather than someone's memory.
	cp config/crd/bases/*.yaml charts/gpucellpool/crds/
	# Same reasoning for the ClusterRole rules. Nothing synced these before, so the
	# kubebuilder markers and the chart's hand-written copy drifted independently —
	# and the chart silently lacked the Cluster API rules it needed.
	sed -n '/^rules:/,$$p' config/rbac/role.yaml | tail -n +2 > charts/gpucellpool/rules.yaml
	$(MAKE) dashboards-sync

.PHONY: dashboards-sync
dashboards-sync: ## Copy the Grafana dashboards into the chart.
	# config/grafana is the source of truth; the chart needs its own copy because
	# Helm can only package files inside the chart directory. `verify` diffs the
	# two, so an edit to one and not the other fails CI instead of shipping a
	# dashboard nobody sees.
	cp config/grafana/*.json charts/gpucellpool/dashboards/

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object paths="./..."

.PHONY: fmt
fmt: ## go fmt.
	go fmt ./...

.PHONY: vet
vet: ## go vet.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run unit + envtest tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint --fix.
	$(GOLANGCI_LINT) run --fix

.PHONY: verify
verify: manifests generate fmt vet ## Fail if generated output is not committed.
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "generated files are out of date — run 'make manifests generate fmt' and commit:"; \
		git status --porcelain; exit 1; \
	fi

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the manager binary.
	go build -o bin/manager ./cmd/manager

.PHONY: run
run: manifests generate fmt vet ## Run the manager against the current kubeconfig.
	go run ./cmd/manager

.PHONY: docker-build
docker-build: ## Build the manager image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the manager image.
	$(CONTAINER_TOOL) push $(IMG)

##@ Deployment

.PHONY: install
install: manifests ## Install CRDs into the current cluster.
	kubectl apply -f config/crd/bases

.PHONY: uninstall
uninstall: manifests ## Remove CRDs from the current cluster.
	kubectl delete --ignore-not-found -f config/crd/bases

##@ Tools

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(LOCALBIN) ## Download controller-gen.
	@test -x $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: envtest
envtest: $(LOCALBIN) ## Download setup-envtest.
	@test -x $(ENVTEST) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN) ## Download golangci-lint.
	@test -x $(GOLANGCI_LINT) || GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
