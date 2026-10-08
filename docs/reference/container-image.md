# `ghcr.io/kelvran/gateway` — container image

The published image of the Kelvran gateway: a self-hosted, OpenAI-compatible LLM gateway in Go (`POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models`) that routes to Bedrock, Anthropic, OpenAI, Gemini and OpenAI-compatible providers, with virtual keys, budgets, rate limits, guardrails, a multi-layer response cache, an admin API and OpenTelemetry built in. Source, issues and the full documentation live at <https://github.com/kelvran/gateway>.

This page is the image's reference card (it is also what Artifact Hub renders via the `io.artifacthub.package.readme-url` label). The operator guide is [`docs/operations/DEPLOY.md`](../operations/DEPLOY.md); the release and verification procedure is [`RELEASE.md`](../../RELEASE.md).

## Tags

| Tag | Meaning |
|---|---|
| `:v<X.Y.Z>` | a release, built from the `gateway/v<X.Y.Z>` git tag; the image's `-version` prints `<X.Y.Z>` |
| `:sha-<40-hex commit>` | every push to `main`; `-version` prints `0.0.0-main.<12-hex>` |
| `:latest` | the most recent push to `main` — moves; pin a digest for anything you deploy |

Pin by index digest, which covers every platform: `docker buildx imagetools inspect ghcr.io/kelvran/gateway:v<X.Y.Z>` prints it.

## Platforms

`linux/amd64` and `linux/arm64`, as one multi-platform index, for every image built on or after 2026-10-08. Earlier images are `linux/amd64` only.

## What is inside

- `FROM scratch`: exactly two files, the static binary at `/gateway` and the CA bundle at `/etc/ssl/certs/ca-certificates.crt`. No shell, no package manager, no libc.
- Runs as UID/GID `65532:65532`; listens on `8080` (`listen_addr` in the config); the admin API listens on a second, separate port when configured.
- `ENTRYPOINT ["/gateway"]`, `CMD ["-config", "/config.yaml"]`: mount your config at `/config.yaml`. `-validate` checks a config and exits; `-version` prints the build identity.
- Provider credentials and other secrets come from environment variables named in the config (`api_key_env`, `access_key_id_env`, …), or from files via the `*_file` fields — never from the image.

## Run

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  -e OPENAI_API_KEY \
  ghcr.io/kelvran/gateway:v<X.Y.Z>

curl -s http://127.0.0.1:8080/healthz          # {"status":"ok"}
```

`docker run --rm ghcr.io/kelvran/gateway:v<X.Y.Z> -version` prints `kelvran-gateway <X.Y.Z> (<commit>, built <date>, go<version>, linux/<arch>)`.

## Verify

The index and each platform manifest are cosign keyless-signed by the repository's GitHub Actions workflow (`cosign sign --recursive`); the CycloneDX SBOM and the SLSA Build Level 2 provenance attestation are attached to the index digest — the digest a tag resolves to and `docker pull` records:

```bash
cosign verify ghcr.io/kelvran/gateway:v<X.Y.Z> \
  --certificate-identity-regexp '^https://github.com/kelvran/gateway/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/kelvran/gateway:v<X.Y.Z> -R kelvran/gateway
```

## Labels

OCI labels (`org.opencontainers.image.title|description|source|documentation|licenses|version|revision|created|url`) and Artifact Hub labels (`io.artifacthub.package.readme-url|license|category|keywords`) are set on the image and, for multi-platform builds, as index annotations: `docker buildx imagetools inspect --raw ghcr.io/kelvran/gateway:v<X.Y.Z>` shows them.

## Health

- `GET /healthz`: liveness, provider-independent, always `200` while the process serves.
- `GET /readyz`: readiness per canonical model (`503` when any configured model has no healthy deployment); point external monitors at it, keep Kubernetes probes on `/healthz` (see `DEPLOY.md` for why).

## License

Apache-2.0 — `LICENSE` and `NOTICE` in the repository.
