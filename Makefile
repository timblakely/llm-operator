# Image URL for building/deploying
IMG ?= controller:dev

# Get the currently used golang install path (in GOPATH/bin, PATH or $home/go/bin)
GO := $(shell which go 2>/dev/null)
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

# CONTAINER_TOOL defines the container tool to be used for building images.
# Ensure to have the builder plugin installed before trying to use it (https://containers.github.io/build/).
CONTAINER_TOOL ?= docker

# Kubebuilder envtest
ENVTEST_ASSETS_DIR := $(shell pwd)/testbin
ENVTEST_K8S_VERSION := 1.31.0

# Tooling versions
CONTROLLER_TOOLS_VERSION := "v0.18.0"
ENVTEST_VERSION := "v0.0.0-20250611183234-8b47372232c1"

.PHONY: all
all: build

.PHONY: build
build: ## Build manager binary.
	$(GO) build -o bin/manager cmd/manager/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	$(GO) run ./cmd/manager/main.go --leader-elect=false

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push $(IMG)

# deployment
.PHONY: deploy
deploy: manifests ## Deploy using kubectl.
	kubectl apply -f config/crd/
	kubectl apply -f config/rbac/
	kubectl apply -f config/manager/manager.yaml

.PHONY: undeploy
undeploy: ## Undeploy using kubectl.
	kubectl delete -f config/crd/
	kubectl delete -f config/rbac/
	kubectl delete -f config/manager/manager.yaml

# Generate
.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests.
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths="./internal/controller/..." output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: controller-gen ## Generate deepcopy code.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: fmt
fmt: ## Format code.
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	$(GO) vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path)" $(GO) test ./... -coverprofile cover.out

.PHONY: migrate
migrate: ## Run configmap-to-crds migration tool.
	$(GO) run ./hack/migration/configmap-to-crds.go --help

# find or download controller-gen
CONTROLLER_GEN = $(shell pwd)/bin/controller-gen
.PHONY: controller-gen
controller-gen:
	$(call go-get-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION))

ENVTEST = $(shell pwd)/bin/setup-envtest
.PHONY: envtest
envtest:
	$(call go-get-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION))

# go-get-tool will 'go version' a tool
go-get-tool:
	GOBIN=$(shell pwd)/bin $(GO) install $(1)

# samples
.PHONY: samples-apply
samples-apply: ## Apply sample CRD instances.
	kubectl apply -f config/samples/

.PHONY: samples-undeploy
samples-undeploy: ## Remove sample CRD instances.
	kubectl delete -f config/samples/ --ignore-not-found=true