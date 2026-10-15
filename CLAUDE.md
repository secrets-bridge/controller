# CLAUDE.md: secrets-bridge/controller

Instructions for Claude Code and contributors working in this repo. For the whole platform, see https://secrets-bridge.io.

## What this repo is

The Secrets Bridge Kubernetes operator (kubebuilder). It reconciles the `SecretsSync` custom resource and validates that its source and destination providers can be built. Value movement is done by the agent, not by this controller.

## Layout

| Path | Purpose |
|---|---|
| `api/v1alpha1` | CRD types (`SecretsSync`) and generated deepcopy code |
| `internal/controller` | Reconciler; the provider registry is injected on the struct |
| `cmd` | Manager entrypoint; registers the `core` providers |
| `config/crd`, `config/rbac`, `config/samples` | Generated CRD, RBAC and sample manifests |

## Build and test

```bash
go build ./... && go vet ./...
go test -race -count=1 ./...
golangci-lint run
go mod tidy   # CI fails on a diff
```

Tests use `controller-runtime`'s fake client with the status subresource enabled.

## Rules for this repo

- Import only `core/providers` and `core/sync`. No Postgres, no Redis, no `api` packages.
- Never read or log secret values.
- Keep RBAC namespaced and minimal. Do not add cluster-wide access to Secrets.
- Status updates ignore 409 conflicts (the next reconcile resolves them); log other errors.
- Keep the CRD's provider list in line with the providers that `core` supports.
- Deployment manifests for real installs live in the `charts` repo; keep `config/` minimal.

## Contributing

Feature branch off `main`, conventional commits, small PRs, CI green, then open a PR for maintainer review. Never push to `main`.

This repo is public. Keep account IDs, hostnames, cluster names, real secret paths and company names out of code, commits, PRs and issues.
