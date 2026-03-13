# k8s-hatch

A lightweight mTLS proxy that gives AI coding agents temporary, permission-restricted access to the Kubernetes API.

Instead of handing an agent a long-lived kubeconfig with broad permissions, k8s-hatch issues a short-lived client certificate signed by an ephemeral CA. The proxy enforces namespace-scoped RBAC and auto-rotates certificates before they expire.

## How it works

1. k8s-hatch runs inside your cluster as a `Deployment` with a `ServiceAccount`
2. On startup it generates a self-signed CA and issues a server cert + client cert
3. It writes a `kubeconfig.yaml` (with embedded client cert) to `/var/hatch/`
4. You retrieve that kubeconfig and hand it to your agent
5. The agent connects over mTLS — requests without a valid client cert are rejected at the TLS layer
6. k8s-hatch proxies requests to the Kubernetes API using the pod's `ServiceAccount` token, scoped to the namespaces you configured via RBAC

Certificates auto-rotate every `certTTL` (default 6h). A new kubeconfig is written on each rotation.

## Quick start

### Install via Helm

```bash
helm upgrade --install k8s-hatch oci://registry-1.docker.io/avin4sh/k8s-hatch \
  --version 0.2.0-chart \
  --namespace k8s-hatch \
  --create-namespace
```

### Get the kubeconfig

```bash
kubectl exec -n k8s-hatch deploy/k8s-hatch -- /k8s-hatch get-kubeconfig > hatch-kubeconfig.yaml
```

### Use it

```bash
kubectl --kubeconfig=hatch-kubeconfig.yaml get pods -n default
```

## Configuration

All options are set via Helm values.

### Image

```yaml
image:
  repository: avin4sh/k8s-hatch
  tag: 0.2.0
  pullPolicy: IfNotPresent
```

### Certificate TTL

```yaml
certTTL: "6h"   # accepts any Go duration string
```

### RBAC — namespaces

Provide a flat list of namespaces. Every namespace gets the same set of rules.

```yaml
rbac:
  namespaces:
    - default
    - my-app
  rules:
    - apiGroups: [""]
      resources: ["pods", "pods/exec", "pods/log"]
      verbs: ["get", "list", "watch", "create"]
    - apiGroups: ["apps"]
      resources: ["deployments", "deployments/scale"]
      verbs: ["get", "list", "patch"]
```

### Exposure modes

**Port-forward (default)**
```bash
kubectl port-forward svc/k8s-hatch -n k8s-hatch 8443:8443
```

**Istio Gateway + VirtualService (TLS passthrough)**
```yaml
istio:
  enabled: true
  hostname: k8s-hatch.example.com

tls:
  extraDNSNames:
    - k8s-hatch.example.com
```

**nginx Ingress (ssl-passthrough)**
```yaml
ingress:
  enabled: true
  hostname: k8s-hatch.example.com
```

**NodePort**
```yaml
service:
  type: NodePort
  nodePort: 30443

tls:
  extraIPAddresses:
    - 192.168.1.10   # node IP
```

### Full values reference

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `k8s-hatch` | Image repository |
| `image.tag` | `latest` | Image tag |
| `certTTL` | `6h` | Client + server cert validity |
| `service.type` | `ClusterIP` | Service type |
| `service.nodePort` | `""` | NodePort value when type=NodePort |
| `tls.extraDNSNames` | `[]` | Extra SANs added to server cert |
| `tls.extraIPAddresses` | `[]` | Extra IP SANs added to server cert |
| `istio.enabled` | `false` | Create Istio Gateway + VirtualService |
| `istio.hostname` | `""` | Hostname for Istio exposure |
| `ingress.enabled` | `false` | Create Ingress |
| `ingress.hostname` | `""` | Hostname for Ingress |
| `rbac.namespaces` | `[default]` | Namespaces to create Role + RoleBinding in |
| `rbac.rules` | see values.yaml | RBAC rules applied to every namespace |

## Building

### Requirements

- Go 1.24+
- Docker with `buildx`
- `helm` 3.x

### Local binary

```bash
make build
```

### Single-platform Docker image

```bash
make docker-build IMAGE=myrepo/k8s-hatch TAG=dev
```

### Multi-arch image (amd64 + arm64) and push

```bash
make docker-buildx TAG=0.2.0
```

### Package and push Helm chart

```bash
make helm-push
```

### Deploy to current kube context

```bash
make deploy IMAGE=myrepo/k8s-hatch TAG=dev
```

### Get kubeconfig from running pod

```bash
make get-kubeconfig
```

## Security model

- The proxy binary runs as UID 65534 (nobody) with a read-only root filesystem
- No shell or utilities are present in the image (`FROM scratch`)
- Clients must present a certificate signed by the hatch CA — unauthenticated connections are dropped at the TLS handshake
- RBAC is namespace-scoped (no `ClusterRole`); the `ServiceAccount` is bound only to the namespaces you list
- Client certificates expire after `certTTL` and are rotated automatically

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for detailed diagrams of the cert lifecycle, transport layering, and Helm deployment topology.
