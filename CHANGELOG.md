# Changelog

All notable changes to `secrets-bridge/controller` are tracked here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project uses [SemVer](https://semver.org/).

Until `v0.1.0` ships, every change lands under `[Unreleased]`. The
rolling `:dev` container tag (published by
`.github/workflows/docker-publish.yml`) reflects whatever is currently
on `main`.

## [Unreleased]

### Security
- `SecretsSync` config keys `token` and `kubernetesTokenPath` are refused
  (`Ready=False`, reason `CredentialConfigRejected`) before any provider
  is built, so a CR can no longer make the controller send a token file
  to an address it names.
- Provider error text no longer goes into `.status.conditions`; status
  gets a generic message and the logs get a one-line summary.
- `spec.refreshInterval` must be at least `30s` (CRD rule, `0s` still
  means the default); the reconciler clamps older objects.
- RBAC trimmed: get/list/watch on `secretssyncs`, update/patch on
  `secretssyncs/status`, no finalizers rule; leader election keeps
  leases (get/create/update) and events only, no ConfigMaps.

### Added
- Docker image publishing workflow — multi-arch (amd64 + arm64),
  pushed to `ghcr.io/secrets-bridge/controller`. Tags: rolling `dev`
  on every push to `main`; semver tags + `latest` activate
  post-`v0.1.0`.

---

## How to cut a release (post-v0.1.0)

1. Land all release-bound work on `main`.
2. Update this file: move `[Unreleased]` entries under a new
   `[vX.Y.Z] — YYYY-MM-DD` heading. Add a fresh empty `[Unreleased]`
   on top.
3. `git tag vX.Y.Z && git push origin vX.Y.Z`.
4. The `docker-publish` workflow picks up the tag and publishes:
   - `ghcr.io/secrets-bridge/controller:vX.Y.Z`
   - `ghcr.io/secrets-bridge/controller:vX.Y`
   - `ghcr.io/secrets-bridge/controller:latest` (only for non-prerelease)
5. Create the GitHub Release pointing at the tag.
