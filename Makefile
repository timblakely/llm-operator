# Image URL for building/deploying
IMG ?= ghcr.io/timblakely/llm-operator:latest

# CONTROLLER_GEN version
CONTROLLER_GEN_VERSION := v0.18.0
SETUP_ENVTEST_VERSION := v0.24.1
ENVTEST_K8S_VERSION := 1.35.0
KUSTOMIZE_VERSION := v5.7.1

# Tools
CONTROLLER_GEN := $(shell pwd)/bin/controller-gen
ENVTEST := $(shell pwd)/bin/setup-envtest
KUSTOMIZE := $(shell pwd)/bin/kustomize

.PHONY: all
all: build

.PHONY: build
build: generate ## Build manager binary.
	go build -o bin/manager cmd/manager/main.go

.PHONY: run
run: manifests generate ## Run controller against the configured Kubernetes cluster.
	go run ./cmd/manager/main.go --leader-elect=false

.PHONY: docker-build
docker-build: ## Build Docker image.
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push Docker image.
	docker push $(IMG)

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate deepcopy code.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt",year=2025 paths="./api/..."

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate CRD and RBAC manifests.
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths="./internal/controller/..." output:rbac:artifacts:config=config/rbac

.PHONY: fmt
fmt: ## Format code.
	go fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if Go source files are not formatted.
	@unformatted="$$(find . -type f -name '*.go' -not -path './vendor/*' -not -path './.git/*' -exec gofmt -l {} +)"; \
	if [ -n "$$unformatted" ]; then echo "unformatted Go files:"; echo "$$unformatted"; exit 1; fi

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(shell pwd)/testbin -p path)" go test -tags=envtest ./... -coverprofile cover.out

.PHONY: unit-test
unit-test: ## Run unit and schema tests without requiring envtest assets.
	go test ./...

.PHONY: envtest-test
envtest-test: envtest ## Run API-server-backed controller tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(shell pwd)/testbin -p path)" go test -tags=envtest ./internal/controller -run '^TestEnvtest' -count=1

.PHONY: manifest-render
manifest-render: $(KUSTOMIZE) ## Render the complete distributable manifest to stdout.
	@$(KUSTOMIZE) build config/default

.PHONY: manifest-validate
manifest-validate: $(KUSTOMIZE) ## Verify the complete distributable manifest renders successfully.
	@$(KUSTOMIZE) build config/default >/dev/null

.PHONY: observation-preflight
observation-preflight: $(KUSTOMIZE) ## Prove the deployment is locked in observation mode.
	@KUSTOMIZE=$(KUSTOMIZE) ./hack/observation-preflight.sh

.PHONY: verify-generated
verify-generated: $(CONTROLLER_GEN) ## Fail if generated CRDs, RBAC, or deepcopy code are stale.
	@CONTROLLER_GEN=$(CONTROLLER_GEN) ./hack/verify-generated.sh

.PHONY: check
check: fmt-check vet unit-test envtest-test manifest-validate observation-preflight verify-generated ## Run all local and CI validation gates.

.PHONY: deploy
deploy: manifests ## Deploy to cluster.
	kubectl apply -k config/default/

.PHONY: undeploy
undeploy: ## Remove from cluster.
	kubectl delete -k config/default/ --ignore-not-found=true

.PHONY: samples-apply
samples-apply: ## Apply sample CRDs.
	kubectl apply -f config/samples/

.PHONY: samples-undeploy
samples-undeploy: ## Remove sample CRDs.
	kubectl delete -f config/samples/ --ignore-not-found=true

.PHONY: migrate
migrate: ## Run configmap-to-crds migration tool.
	go run ./hack/migration/configmap-to-crds.go --help

$(CONTROLLER_GEN):
	GOBIN=$(shell pwd)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

$(KUSTOMIZE):
	GOBIN=$(shell pwd)/bin go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest tools.
$(ENVTEST):
	GOBIN=$(shell pwd)/bin go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)
