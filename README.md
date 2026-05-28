# secrets-bridge / controller

**Kubernetes operator + CRDs for [Secrets Bridge](https://github.com/secrets-bridge)** — written with kubebuilder + controller-runtime. Reconciles the `sync.secrets-bridge.io/v1alpha1` `SecretsSync` CRD and dispatches sync work via the [`core`](https://github.com/secrets-bridge/core) provider abstraction.

## Status

| Issue | Step | Status |
|---|---|---|
| [#1](https://github.com/secrets-bridge/controller/issues/1) | Migrate v0.1.0 operator onto core | **this PR** |
| [#2](https://github.com/secrets-bridge/controller/issues/2) | GitOps CRD integration (Flow 4) | open |

## Architecture

The controller imports **only** `core/providers` — never `api/pkg/storage`, `api/pkg/runtime`, or any Control Plane internal — per the polyrepo dependency rule. Reading and writing actual secret values is the **agent**'s job per BRD §12.4; this controller:

1. Watches `SecretsSync` CRs
2. Validates the CR by resolving source + destination providers from the Registry
3. Surfaces a `Ready` condition on `.status.conditions`
4. Re-queues every `spec.refreshInterval` (default 5m)

Once the agent registration loop ([secrets-bridge/agent#1](https://github.com/secrets-bridge/agent/issues/1)) and the job loop ([#2](https://github.com/secrets-bridge/agent/issues/2)) land, the controller will dispatch sync jobs to the Control Plane API which the agent then claims and executes inside the target boundary.

## CRD

`SecretsSync` is **cluster-scoped** (one CR per source ↔ destination pair). Spec shape:

```yaml
apiVersion: sync.secrets-bridge.io/v1alpha1
kind: SecretsSync
metadata:
  name: vault-to-aws-mirror
spec:
  source:
    type: vault
    config:
      address: https://vault.example.com
      authMethod: kubernetes
      kubernetesRole: secrets-bridge
      kvMount: kv
      kvPrefix: apps
  destination:
    type: aws-sm
    config:
      region: us-east-1
  direction: SourceToDestination
  refreshInterval: 5m
```

See [`core/providers/{vault,awssecretsmanager}`](https://github.com/secrets-bridge/core/tree/main/providers) for the full list of accepted config keys per provider.

## Layout

```
cmd/                       manager entrypoint + flags + provider registration
api/v1alpha1/              CRD types (SecretsSync) + deepcopy + scheme
internal/controller/       reconciler implementation
config/crd/                generated CustomResourceDefinition
config/rbac/               role / role binding / service account for the operator
config/samples/            example SecretsSync CR
```

## Hard rules (per CLAUDE.md)

- Imports `core/providers` only. **No** `api/pkg/storage`, **no** Redis client, **no** Postgres driver.
- No secret values in CR status, conditions, events, or logs.
- Operator runs as `nonroot` on `distroless/static`.

## Local development

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
```

## Container

```bash
docker build -t secrets-bridge-controller:dev .
```

Multi-stage build on `golang:1.26-alpine` → `distroless/static:nonroot`. No shell, no package manager.
