REGISTRY       ?= avin4sh
IMAGE          ?= k8s-hatch
TAG            ?= latest
CHART          := charts/k8s-hatch
CHART_REGISTRY ?= oci://registry-1.docker.io/$(REGISTRY)
PLATFORMS      ?= linux/amd64,linux/arm64

.PHONY: build docker-build docker-buildx helm-push kind-load deploy undeploy port-forward get-kubeconfig test-e2e

build:
	go build -o bin/k8s-hatch .

docker-build:
	docker build -t $(IMAGE):$(TAG) .

docker-buildx:
	docker buildx build --platform $(PLATFORMS) --push -t $(REGISTRY)/$(IMAGE):$(TAG) .

helm-push:
	helm package $(CHART) -d /tmp
	helm push /tmp/k8s-hatch-$(shell helm show chart $(CHART) | grep '^version:' | awk '{print $$2}').tgz $(CHART_REGISTRY)

kind-load:
	kind load docker-image $(IMAGE):$(TAG)

deploy:
	helm upgrade --install k8s-hatch $(CHART) \
	  --set image.repository=$(IMAGE) --set image.tag=$(TAG)

undeploy:
	helm uninstall k8s-hatch

port-forward:
	kubectl port-forward svc/k8s-hatch 8443:8443

# Reads kubeconfig from container filesystem via exec
get-kubeconfig:
	kubectl exec deploy/k8s-hatch -- /k8s-hatch get-kubeconfig > hatch-kubeconfig.yaml
	@echo "Written to hatch-kubeconfig.yaml"

test-e2e:
	go test ./e2e/ -v -timeout 5m
