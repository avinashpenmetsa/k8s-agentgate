REGISTRY       ?= avin4sh
IMAGE          ?= k8-agentgate
TAG            ?= latest
CHART          := charts/k8-agentgate
CHART_REGISTRY ?= oci://registry-1.docker.io/$(REGISTRY)
PLATFORMS      ?= linux/amd64,linux/arm64

.PHONY: build docker-build docker-buildx helm-push kind-load deploy undeploy port-forward get-kubeconfig test-e2e

build:
	go build -o bin/k8-agentgate .

docker-build:
	docker build -t $(IMAGE):$(TAG) .

docker-buildx:
	docker buildx build --platform $(PLATFORMS) --push -t $(REGISTRY)/$(IMAGE):$(TAG) .

helm-push:
	helm package $(CHART) -d /tmp
	helm push /tmp/k8-agentgate-$(shell helm show chart $(CHART) | grep '^version:' | awk '{print $$2}').tgz $(CHART_REGISTRY)

kind-load:
	kind load docker-image $(IMAGE):$(TAG)

deploy:
	helm upgrade --install k8-agentgate $(CHART) \
	  --set image.repository=$(IMAGE) --set image.tag=$(TAG)

undeploy:
	helm uninstall k8-agentgate

port-forward:
	kubectl port-forward svc/k8-agentgate 8443:8443

# Reads kubeconfig from container filesystem via exec
get-kubeconfig:
	kubectl exec deploy/k8-agentgate -- /k8-agentgate get-kubeconfig > agentgate-kubeconfig.yaml
	@echo "Written to agentgate-kubeconfig.yaml"

test-e2e:
	go test ./e2e/ -v -timeout 5m
