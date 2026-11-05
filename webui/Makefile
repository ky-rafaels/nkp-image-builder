# Build/publish the NKP image-builder web UI and the image-builder job image.
#
#   make push   REGISTRY=registry.example.com/nkp TAG=v0.1.0
#   make deploy REGISTRY=registry.example.com/nkp TAG=v0.1.0 VALUES=my-values.yaml

REGISTRY ?= localhost:5000
TAG ?= $(shell git describe --tags --always --dirty)
PLATFORM ?= linux/amd64
WEBUI_IMAGE ?= $(REGISTRY)/nkp-image-builder-webui:$(TAG)
BUILDER_IMAGE ?= $(REGISTRY)/nkp-image-builder:$(TAG)
NAMESPACE ?= nkp-image-builder
RELEASE ?= nkp-image-builder
CHART := charts/nkp-image-builder
VALUES ?=

.PHONY: help
help: ## Show targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-16s\033[0m %s\n",$$1,$$2}'

.PHONY: test
test: ## Run unit tests
	go vet ./... && go test -race ./...

.PHONY: run
run: ## Run locally against the current kubeconfig (uses hack/dev-config.json)
	go run . -addr :8080 -config hack/dev-config.json -kubeconfig $${KUBECONFIG:-$$HOME/.kube/config}

.PHONY: image-webui
image-webui: ## Build the web UI image
	docker build --platform $(PLATFORM) -t $(WEBUI_IMAGE) .

.PHONY: image-builder
image-builder: ## Build the image-builder job image from ../images/capi (packer + ansible + nutanix plugin)
	docker build --platform $(PLATFORM) --build-arg PASSED_IB_VERSION=$(TAG) -t $(BUILDER_IMAGE) ../images/capi

.PHONY: images
images: image-webui image-builder ## Build both images

.PHONY: push
push: images ## Build and push both images
	docker push $(WEBUI_IMAGE)
	docker push $(BUILDER_IMAGE)

.PHONY: lint-chart
lint-chart: ## Lint and render the Helm chart
	helm lint $(CHART) --set nutanix.credentials.username=x --set nutanix.credentials.password=x --set nutanix.credentials.endpoint=pc
	helm template $(RELEASE) $(CHART) -n $(NAMESPACE) --set nutanix.credentials.username=x --set nutanix.credentials.password=x --set nutanix.credentials.endpoint=pc > /dev/null

.PHONY: deploy
deploy: ## Install/upgrade the chart pointing at the pushed images
	helm upgrade --install $(RELEASE) $(CHART) -n $(NAMESPACE) --create-namespace \
		--set image.repository=$(REGISTRY)/nkp-image-builder-webui --set image.tag=$(TAG) \
		--set builder.image.repository=$(REGISTRY)/nkp-image-builder --set builder.image.tag=$(TAG) \
		$(if $(VALUES),-f $(VALUES))
