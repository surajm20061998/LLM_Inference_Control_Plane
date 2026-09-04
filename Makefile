# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR substitutes the YEAR placeholder in hack/boilerplate.go.txt.
#
# PINNED, not read from the clock. CI regenerates everything and fails the build
# if `git diff` is non-empty, so a clock-derived year turns 1 January into a
# repo-wide false failure: every open pull request starts reporting that its
# unrelated change made the manifests stale, and the only "fix" is to commit a
# year bump that rewrites the header on files nobody touched.
YEAR ?= 2026

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	@# Resolved to an ABSOLUTE path. `setup-envtest ... -p path` prints a path
	@# relative to the working directory, and `go test` runs each package with
	@# cwd set to that package's own directory — so a relative KUBEBUILDER_ASSETS
	@# points somewhere that does not exist and every envtest spec fails to find
	@# etcd, with an error that looks like a broken install rather than a broken
	@# variable.
	KUBEBUILDER_ASSETS="$$(cd "$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" && pwd)" \
		go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= llmcp-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	@# Builds Dockerfile directly. The scaffolded version of this target copied
	@# the Dockerfile aside and sed'd `--platform=$${BUILDPLATFORM}` into its
	@# first FROM, because the Dockerfile did not carry it. It does now — like
	@# every other image in this repo — so the sed would insert a SECOND
	@# --platform flag on a line that already has one.
	- $(CONTAINER_TOOL) buildx create --name llmcp-builder
	$(CONTAINER_TOOL) buildx use llmcp-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile .
	- $(CONTAINER_TOOL) buildx rm llmcp-builder

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.12.2
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): | $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): | $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): | $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): | $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@# The plugin-enabled build REPLACES the versioned file and the symlink is
	@# repointed at it, so go-install-tool's `readlink` cache guard keeps
	@# hitting. Writing a plain file over the symlink — which is what the
	@# previous `mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT)` did —
	@# made `readlink` return empty, so the guard missed forever and every lint
	@# invocation rebuilt both the tool and its plugins from source.
	@#
	@# The stamp is what stops the plugin build itself from re-running: it is
	@# keyed on the version and invalidated by .custom-gcl.yml being newer.
	@#
	@# No `|| true`. Swallowing a failure here leaves the PLAIN binary in place,
	@# and the next `golangci-lint run` dies with
	@#   build linters: plugin(logcheck): plugin "logcheck" not found
	@# which blames .golangci.yml's linter list rather than the build step that
	@# actually broke, with the real error long gone. `make lint-config` does
	@# not catch it either — `config verify` on the plain binary exits 0.
	@if [ -f .custom-gcl.yml ]; then \
		stamp="$(LOCALBIN)/.golangci-lint-custom-$(GOLANGCI_LINT_VERSION).stamp"; \
		if [ ! -f "$$stamp" ] || [ .custom-gcl.yml -nt "$$stamp" ]; then \
			set -e; \
			echo "Building custom golangci-lint with plugins..."; \
			"$(GOLANGCI_LINT)" custom --destination "$(LOCALBIN)" --name golangci-lint-custom; \
			mv -f "$(LOCALBIN)/golangci-lint-custom" "$(GOLANGCI_LINT)-$(GOLANGCI_LINT_VERSION)"; \
			ln -sf "$$(realpath "$(GOLANGCI_LINT)-$(GOLANGCI_LINT_VERSION)")" "$(GOLANGCI_LINT)"; \
			touch "$$stamp"; \
		fi; \
	fi

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

##@ Local development loop (kind + local registry)

# Everything below targets the local kind cluster created by
# hack/kind-with-registry.sh. Images are pushed to a registry on the kind
# network rather than side-loaded with `kind load`, because a registry push is
# layer-incremental — which matters a great deal once a model image is in play.

CLUSTER_NAME ?= llmcp
REGISTRY ?= localhost:5001

CONTROLLER_IMG ?= $(REGISTRY)/llmcp-controller:dev
SHIM_IMG       ?= $(REGISTRY)/llmcp-shim:dev
FAKEENGINE_IMG ?= $(REGISTRY)/llmcp-fakeengine:dev
LOADGEN_IMG    ?= $(REGISTRY)/llmcp-loadgen:dev
MODEL_IMG      ?= $(REGISTRY)/llmcp-model:dev

# The real weights get their OWN tag, for two reasons. It keeps `make
# dev-images` — which rebuilds the placeholder model image on every fast
# iteration — from clobbering a 400 MB artifact it knows nothing about. And
# because the tag names the exact model and quantisation it contains, it is
# immutable in practice, so the samples that use it can honestly say
# pullPolicy: IfNotPresent.
MODEL_REAL_IMG ?= $(REGISTRY)/llmcp-model:qwen3-0.6b-q4km

# Path to a real model file to package into MODEL_IMG. When empty, a small
# placeholder is used, which is all the deterministic stub engine needs.
MODEL_FILE ?=

# The real model. Qwen3-0.6B at Q4_K_M is 397 MB, Apache-2.0 and ungated, which
# is what makes it a model this project can pull in CI without a token and run
# three replicas of on a laptop.
#
# MODEL_SHA256 is not decoration. Weights arrive over the network and are then
# baked into an image that every serving pod loads; a truncated download
# produces a GGUF that fails to parse inside the engine, minutes later, with an
# error that points at the model rather than at the transfer. Verifying here
# turns that into an immediate, obvious failure.
MODEL_REPO   ?= unsloth/Qwen3-0.6B-GGUF
MODEL_GGUF   ?= Qwen3-0.6B-Q4_K_M.gguf
MODEL_SHA256 ?= ac2d97712095a558e31573f62f466a3f9d93990898b0ec79d7c974c1780d524a
MODEL_CACHE  ?= .build/models

# Stamped into the shim's llmcp_shim_info gauge. It is how a running fleet can
# be checked for skew between the shim emitting metrics and the controller
# querying them — a skew whose only other symptom is an empty query result.
SHIM_VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: kind-up
kind-up: ## Create the local kind cluster with an attached image registry.
	@bash hack/kind-with-registry.sh

.PHONY: kind-down
kind-down: ## Delete the local kind cluster (the registry container is left running).
	@kind delete cluster --name $(CLUSTER_NAME)

.PHONY: model-download
model-download: ## Download the real GGUF weights into $(MODEL_CACHE) (cached; safe to re-run).
	@mkdir -p $(MODEL_CACHE)
	@if [ -f "$(MODEL_CACHE)/$(MODEL_GGUF)" ]; then \
		echo "Already downloaded: $(MODEL_CACHE)/$(MODEL_GGUF)"; \
	else \
		echo "Downloading $(MODEL_REPO)/$(MODEL_GGUF) (~400 MB)..."; \
		curl -fL --retry 3 --progress-bar \
			-o "$(MODEL_CACHE)/$(MODEL_GGUF).partial" \
			"https://huggingface.co/$(MODEL_REPO)/resolve/main/$(MODEL_GGUF)?download=true"; \
		mv "$(MODEL_CACHE)/$(MODEL_GGUF).partial" "$(MODEL_CACHE)/$(MODEL_GGUF)"; \
	fi
	@echo "Verifying checksum..."
	@echo "$(MODEL_SHA256)  $(MODEL_CACHE)/$(MODEL_GGUF)" | shasum -a 256 -c - \
		|| { echo "CHECKSUM MISMATCH. Delete $(MODEL_CACHE)/$(MODEL_GGUF) and retry, or update MODEL_SHA256 if the upstream file legitimately changed."; exit 1; }

.PHONY: model-image-real
model-image-real: model-download ## Build and push the model image containing the real Qwen3 weights.
	$(MAKE) model-image MODEL_FILE=$(MODEL_CACHE)/$(MODEL_GGUF) MODEL_IMG=$(MODEL_REAL_IMG)

.PHONY: model-image
model-image: ## Build and push the model image. Pass MODEL_FILE=<path> to package real weights.
	@rm -rf .build/model && mkdir -p .build/model
	@if [ -n "$(MODEL_FILE)" ]; then \
		echo "Packaging $(MODEL_FILE)"; \
		cp "$(MODEL_FILE)" .build/model/model.gguf; \
	else \
		echo "No MODEL_FILE given; packaging a placeholder"; \
		echo "llmcp placeholder model - no weights packaged" > .build/model/model.gguf; \
	fi
	$(CONTAINER_TOOL) build -f Dockerfile.model -t $(MODEL_IMG) .build/model
	$(CONTAINER_TOOL) push $(MODEL_IMG)

.PHONY: fakeengine-image
fakeengine-image: ## Build and push the deterministic stub engine image.
	$(CONTAINER_TOOL) build -f Dockerfile.fakeengine -t $(FAKEENGINE_IMG) .
	$(CONTAINER_TOOL) push $(FAKEENGINE_IMG)

.PHONY: loadgen-image
loadgen-image: ## Build and push the streaming load generator image.
	$(CONTAINER_TOOL) build -f Dockerfile.loadgen -t $(LOADGEN_IMG) .
	$(CONTAINER_TOOL) push $(LOADGEN_IMG)

.PHONY: controller-image
controller-image: ## Build and push the operator image.
	$(CONTAINER_TOOL) build -t $(CONTROLLER_IMG) .
	$(CONTAINER_TOOL) push $(CONTROLLER_IMG)

.PHONY: shim-image
shim-image: ## Build and push the metrics sidecar image.
	$(CONTAINER_TOOL) build -f Dockerfile.shim --build-arg VERSION=$(SHIM_VERSION) -t $(SHIM_IMG) .
	$(CONTAINER_TOOL) push $(SHIM_IMG)

.PHONY: dev-images
dev-images: controller-image shim-image fakeengine-image model-image ## Build and push every development image.

.PHONY: dev-deploy
dev-deploy: manifests kustomize dev-images ## Install CRDs and deploy the operator to the kind cluster.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(CONTROLLER_IMG)
	$(KUSTOMIZE) build config/dev | $(KUBECTL) apply -f -
	$(KUBECTL) -n llmcp-system rollout restart deploy/llmcp-controller-manager
	$(KUBECTL) -n llmcp-system rollout status deploy/llmcp-controller-manager --timeout=180s

.PHONY: dev-undeploy
dev-undeploy: kustomize ## Remove the operator and CRDs from the kind cluster.
	-$(KUSTOMIZE) build config/dev | $(KUBECTL) delete --ignore-not-found -f -
	-$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found -f -

.PHONY: dev-reset
dev-reset: ## Tear the cluster down and rebuild it from scratch.
	-$(MAKE) kind-down
	$(MAKE) kind-up

##@ Observability (kube-prometheus-stack)

HELM ?= helm

# Pinned. The chart's ServiceMonitor selector semantics have changed between
# major versions, and this project's values file depends on the current ones —
# an unpinned chart is a demo that silently stops collecting metrics on an
# upgrade nobody made deliberately.
KPS_CHART_VERSION ?= 88.6.2
KPS_RELEASE       ?= kps
KPS_NAMESPACE     ?= monitoring
KPS_VALUES        ?= hack/values/kube-prometheus-stack.yaml

# Where the sprint-3 exit criterion is checked from.
PROM_URL ?= http://localhost:9091

.PHONY: monitoring-install
monitoring-install: kustomize ## Install kube-prometheus-stack and wire the operator's own metrics in.
	@# --force-update, because a plain `helm repo add` FAILS when the repo is
	@# already configured with a different URL — and .SHELLFLAGS is -ec, so that
	@# aborts the target. Harmless on a fresh runner, hostile on a dev machine.
	$(HELM) repo add --force-update prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
	$(HELM) repo update prometheus-community >/dev/null
	$(HELM) upgrade --install $(KPS_RELEASE) prometheus-community/kube-prometheus-stack \
		--namespace $(KPS_NAMESPACE) --create-namespace \
		--version $(KPS_CHART_VERSION) \
		-f $(KPS_VALUES) \
		--wait --timeout 10m
	@# Applied only now that the Helm release has created the
	@# monitoring.coreos.com CRDs. Kept out of config/dev for exactly that
	@# reason — see config/monitoring/kustomization.yaml.
	$(KUSTOMIZE) build config/monitoring | $(KUBECTL) apply -f -
	@echo
	@echo "Grafana:    make grafana     (admin / admin)"
	@echo "Prometheus: make prometheus"

.PHONY: monitoring-uninstall
monitoring-uninstall: ## Remove kube-prometheus-stack.
	-$(HELM) uninstall $(KPS_RELEASE) --namespace $(KPS_NAMESPACE)

.PHONY: grafana
grafana: ## Port-forward Grafana to http://localhost:3000 (admin/admin).
	@echo "Grafana on http://localhost:3000 — admin / admin"
	$(KUBECTL) -n $(KPS_NAMESPACE) port-forward svc/$(KPS_RELEASE)-grafana 3000:80

.PHONY: prometheus
prometheus: ## Port-forward Prometheus to http://localhost:9091.
	@echo "Prometheus on $(PROM_URL)"
	$(KUBECTL) -n $(KPS_NAMESPACE) port-forward svc/$(KPS_RELEASE)-kube-prometheus-stack-prometheus 9091:9090

.PHONY: verify-ttft
verify-ttft: ## Sprint 3 exit criterion: assert ttft_p95 < 0.5 x duration_p95 in Prometheus.
	@PROM_URL=$(PROM_URL) bash hack/verify/ttft-ratio.sh

##@ End-to-end (Chainsaw, needs a real cluster)

# Pinned. KUTTL is stagnant; Chainsaw is the maintained successor and its
# assertion semantics have changed between minor versions.
CHAINSAW_VERSION ?= v0.2.15
CHAINSAW ?= $(LOCALBIN)/chainsaw

# Which suite to run. Empty runs all of them.
SUITE ?=

.PHONY: chainsaw
chainsaw: $(CHAINSAW) ## Download chainsaw locally if necessary.
$(CHAINSAW): | $(LOCALBIN)
	$(call go-install-tool,$(CHAINSAW),github.com/kyverno/chainsaw,$(CHAINSAW_VERSION))

.PHONY: chainsaw-lint
chainsaw-lint: chainsaw ## Validate the Chainsaw suites without a cluster.
	@# Worth having as its own target, and worth being in `make verify`.
	@#
	@# Chainsaw suites are the one tier that cannot run without a cluster, so
	@# they are also the one tier where an authoring mistake — a misspelled
	@# field, an `assert` where a `try` belongs — can sit undetected until
	@# somebody has a cluster AND time. `chainsaw lint` validates them against
	@# Chainsaw's own schema offline, which eliminates that whole class for the
	@# price of a second.
	@#
	@# It does NOT prove the suites pass. It proves they are suites.
	@set -e; \
	"$(CHAINSAW)" lint configuration -f test/chainsaw/config.yaml >/dev/null; \
	echo "config.yaml: valid"; \
	for f in test/chainsaw/*/chainsaw-test.yaml; do \
		"$(CHAINSAW)" lint test -f "$$f" >/dev/null || { echo "INVALID: $$f"; exit 1; }; \
		echo "$$f: valid"; \
	done

.PHONY: e2e-chainsaw
e2e-chainsaw: chainsaw ## Run the Chainsaw suites against the current cluster.
	@if [ -n "$(SUITE)" ]; then \
		"$(CHAINSAW)" test "test/chainsaw/$(SUITE)" --config test/chainsaw/config.yaml; \
	else \
		"$(CHAINSAW)" test test/chainsaw --config test/chainsaw/config.yaml; \
	fi

.PHONY: e2e-up
e2e-up: kind-up dev-images dev-deploy ## Bring up a cluster with the operator and the stub images.
	@echo "Cluster ready. Run: make e2e-chainsaw"

.PHONY: verify
verify: manifests generate fmt vet lint test api-docs-check chainsaw-lint ## Everything that runs without a cluster.

##@ Documentation

CRD_REF_DOCS_VERSION ?= v0.3.0
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Download crd-ref-docs locally if necessary.
$(CRD_REF_DOCS): | $(LOCALBIN)
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

.PHONY: api-docs
api-docs: crd-ref-docs ## Generate docs/api.md from the Go type comments.
	$(CRD_REF_DOCS) 		--source-path=./api 		--config=hack/crd-ref-docs.yaml 		--renderer=markdown 		--output-path=docs/api.md

.PHONY: api-docs-check
api-docs-check: api-docs ## Fail if docs/api.md is out of date.
	@# Two different states, deliberately distinguished.
	@#
	@# UNTRACKED means the file has simply never been committed. That is repo
	@# hygiene, not drift, and failing on it would make `make verify` red in a
	@# perfectly good working tree — which trains people to stop running it.
	@#
	@# TRACKED AND MODIFIED means regenerating changed the output, so the
	@# committed reference no longer matches the Go types it documents. That
	@# is the failure worth blocking on, because API docs are read by people
	@# who cannot check them against the source.
	@if ! git ls-files --error-unmatch docs/api.md >/dev/null 2>&1; then \
		echo "docs/api.md is not tracked yet; generated it, nothing to compare against."; \
	elif ! git diff --quiet -- docs/api.md; then \
		echo "docs/api.md is out of date. Run 'make api-docs' and commit the result."; \
		git --no-pager diff -- docs/api.md | head -40; \
		exit 1; \
	else \
		echo "docs/api.md is up to date."; \
	fi

.PHONY: load
load: ## Run the in-cluster streaming load generator against MD=<name>.
	@MD=$(MD) NAMESPACE=$(NAMESPACE) LOADGEN_IMG=$(LOADGEN_IMG) bash hack/load/run-load.sh
