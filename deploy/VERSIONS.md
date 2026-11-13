# Pinned versions

Every image and tool used by the local stack, pinned to an exact tag and, for images,
to the multi-arch index digest (`tag@sha256:...`). Update tag and digest together.

| Component | Tag | Digest | Used in |
| --- | --- | --- | --- |
| Go | `golang:1.27.4-alpine3.24` | `sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf` | `go.mod` (1.27), `Dockerfile.geoint`, `Dockerfile.sourcesim`, `compose.dev.yml` |
| Final runtime image (no shell, user 65532) | `gcr.io/distroless/static:nonroot` | `sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329` | `Dockerfile.geoint`, `Dockerfile.sourcesim` |
| PostgreSQL 18.6 | `postgres:18.6-alpine3.24` | `sha256:9ba53c7d41ea093baf5c1ba635ba5434c4f979880527d49bb7d9237d3193085b` | `compose.yml` |
| NATS (JetStream) 2.15.2 | `nats:2.15.2-alpine` | `sha256:afae2fdbe2e326c65e1246cb83b9d05d319188734f3bd577ac8072daae78a10f` | `compose.yml` |
| MinIO | `ghcr.io/coollabsio/minio:RELEASE.2025-10-15T17-29-55Z` | `sha256:69b55a1c1c5dc285ce04db96689f5b2102317fc77a50680a1874ca6efd1c87f9` | `compose.yml` |
| Keycloak 26.8.2 | `quay.io/keycloak/keycloak:26.8.2` | `sha256:723b1d6e254c0fc96d5cffec860c7e0744d6a3277822b865bb538cf891575e41` | `compose.yml` |
| Alpine helper (cache volumes, `volume-init`, backup/restore, `make test-scripts`) | `alpine:3.24` | `sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf` | `Makefile`, `compose.yml`, `scripts/backup.sh`, `scripts/restore.sh` |
| Go with glibc and gcc (`make test-race` only) | `golang:1.27.4-trixie` | `sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9` | `compose.dev.yml` (`go-race`) |
| Node.js 24 LTS (web build, `make test-web` / `lint-web` / `audit-web` / `build-web`) | `node:24.22.1-alpine3.24` | `sha256:06d7adf3e0ef61a13f1d28682726366f97a15aa1888c934d3ecec86c33918b53` | `Dockerfile.web`, `Makefile` |
| Web server (non-root, uid 101) | `nginxinc/nginx-unprivileged:1.30.6-alpine3.24` | `sha256:f5116ed4606f3eb015dd4a8e28cb9b351476dc0ad4bc6e9f93b19cec130cdf03` | `Dockerfile.web` |
| govulncheck | `v1.9.0` | n/a (Go module) | `Makefile` |
| Playwright 1.64.1 with its browsers (Ubuntu 24.04; `make e2e` only) | `mcr.microsoft.com/playwright:v1.64.1-noble` | `sha256:2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f` | `compose.e2e.yml` |
| `@playwright/test` (exact, must equal the image's version) | `1.64.1` | n/a (npm, `web/package-lock.json`) | `web/package.json` |
| actions/checkout | `v4.3.1` (commit `34e114876b0b11c390a56381ad16ebd13914f8d5`) | n/a | `.github/workflows/ci.yml` |
| actions/upload-artifact | `v4.6.2` (commit `ea165f8d65b6e75b540449e92b4886f43607fa02`) | n/a | `.github/workflows/ci.yml` (e2e results on failure) |
| `@types/node` (exact, Node 24 major; types for `web/e2e`) | `24.19.2` | n/a (npm, `web/package-lock.json`) | `web/package.json` |

Notes:

- PostgreSQL runs with explicit settings (`compose.yml` command line and
  `shm_size`), sized for the reference machine and overridable from `.env`:
  see [`TUNING.md`](TUNING.md). Every performance number must state the
  settings it was measured with.
- Go module `golang.org/x/crypto` (indirect): `v0.59.0`, the first tagged
  release after the fix of GO-2026-5932 (openpgp; the package is not imported
  by this project, govulncheck reported it at module level). Until 2026-11-11
  the project pinned the untagged pseudo-version
  `v0.58.1-0.20261009181718-2f1f834330b2` for that fix. v0.59.0 was one day
  old when adopted (2026-11-12): younger than the two weeks used for the
  Playwright pin, accepted because it replaces an untagged commit of the same
  branch; `make vuln` reports no vulnerability with it.
- Containers: every service drops all capabilities and runs with
  `no-new-privileges`. postgres runs as the image's user 70, nats and minio
  as 10001, keycloak as the image's 1000, api/worker as 65532, web as 101.
  The one-shot `volume-init` (alpine, root with CHOWN and DAC_READ_SEARCH
  only, no network) gives the data volumes to those users before they start.
  Read-only root filesystems: api, worker, migrate, web, nats, minio, postgres
  and volume-init; not keycloak (`start-dev` rebuilds its server under
  `/opt/keycloak` at every start).
- Logs: json-file driver limited to 5 files of 20 MB per container.

- MinIO: upstream no longer publishes images (`minio/minio`, `quay.io/minio/minio`
  are gone) and the project is frozen. The image above is a third-party community
  build of a frozen upstream release. That is acceptable for local use only; before
  any hosted deployment, replace it with a maintained S3-compatible store or a build
  from source.
- nats 2.15.3 has no image for this platform at the time of writing, hence 2.15.2.
- Keycloak runs in dev mode (plain HTTP, local machine only) with its own embedded
  store (volume `kcdata`), not the postgres server, and has no healthcheck. It
  imports `deploy/keycloak/realm-geoint.json` at start (`--import-realm`); the
  import is skipped when the realm already exists in `kcdata`. Hostname v2 options
  (`KC_HOSTNAME` as a full URL + `KC_HOSTNAME_BACKCHANNEL_DYNAMIC`) were checked
  against `kc.sh start-dev --help` of 26.8.2.
- The api image has no shell, so it has no container healthcheck; `make up` polls
  `http://api:8080/healthz` from the web container (busybox wget, 2 s per try),
  so the host needs no curl.
- Published ports (api 8080, keycloak 8081, web 5173) are bound to 127.0.0.1.
- Web app (`web` service, port 5173 on 127.0.0.1): npm dependencies are pinned to
  exact versions in `web/package.json` with `web/package-lock.json` committed, and
  installed with `npm ci --ignore-scripts`; tools run with `npm exec --no`. The
  final image is nginx-unprivileged. It keeps a shell because its start-up script
  (`web/docker/web-config.sh`) validates the `WEB_*` variables and renders
  `/config.json` and the CSP; an invalid value stops the container.
- End-to-end test (`make e2e`, `deploy/compose.e2e.yml`): the Playwright image
  runs the browser test on the e2e stack's network; its version must be the
  exact `@playwright/test` version of `web/package.json` (the browsers in the
  image are built for it). 1.64.1 was the newest release at least two weeks
  old on 2026-11-12 (1.65.0 was a week old). The source simulator
  (`deploy/Dockerfile.sourcesim`, `cmd/sourcesim`) is built from the same Go
  and distroless images as `geoint` and exists only in that stack.
