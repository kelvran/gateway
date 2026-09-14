# Performance, Reliability & Edge/Secrets Hardening — Upgrade Research (2026-09-14)

Recovered by hand from the deep-research workflow's own returned JSON — its file-write step failed
silently, a known recurring class per `AGENTS.md`'s Gotchas section. Reconstructed from the
workflow's `summary`/`findings` fields, not re-run.

## Headline correction, found mid-research

This report's own RQ1 (connection pooling/HTTP-2 to upstream providers) assumed an unexamined
default. Direct inspection during synthesis found it was already fixed by a SEPARATE, earlier
same-day research pass, `docs/upgrade-research/performance-latency-optimization-2026-09-14.md`
(Finding 1, 15:59): `gateway/cmd/gateway/main.go`'s `newUpstreamTransport()` already raises
`MaxIdleConnsPerHost` from Go's stdlib default of 2 to 100 (`const upstreamMaxIdleConnsPerHost =
100`), proven by `TestNewUpstreamTransportRaisesMaxIdleConnsPerHost`. It also clones
`http.DefaultTransport` rather than building a bare `&http.Transport{}`, preserving
`ForceAttemptHTTP2: true` — so HTTP/2 multiplexing to upstream providers is already active. No
action needed on RQ1.

## Findings

### RQ1 — Outbound connection pooling + HTTP/2 — already shipped, no gap

See correction above. Both the idle-connection-pool sizing and HTTP/2 multiplexing to upstream
providers are already correctly configured.

### RQ2 — Adaptive/latency-aware routing — `not_yet`

A real, low-risk, non-ML precedent exists — Envoy's built-in Least-Request/P2C policy — but it is
gated on the same production-traffic-volume floor Kelvran has already and separately named for its
retry-budget and bandit-routing deferrals (`gateway/internal/router/router.go`,
`docs/rfcs/2026-09-04-weighted-routing.md`). Peak EWMA is a real alternative but is self-labeled
"experimental" by its own maintainer (Twitter/Finagle) and shouldn't be treated as a default.

**Verdict: not_yet.** Named trigger: real production traffic volume sufficient to validate a
load-aware policy against Kelvran's own static-weight WRR baseline — unchanged from the existing
retry-budget/bandit-routing deferral's own trigger.

### RQ3 — Client-facing HTTP/2/HTTP-3 — premature, deeper prerequisite gap found

Genuinely premature: Kelvran's client-facing listener (`gateway/cmd/gateway/main.go`) runs plain,
un-TLS'd HTTP/1.1 via `server.ListenAndServe()` — no `TLSConfig`, no h2c wiring, and no Kubernetes
Ingress/LoadBalancer manifest anywhere in `deploy/k8s/base/`. TLS termination, the prerequisite for
either HTTP/2 or HTTP/3 on this surface, doesn't exist yet. (This may be intentional — TLS
termination at a reverse proxy/load balancer in front of the gateway is a common deployment
pattern — but no such manifest confirms that's the plan either.)

**Verdict: not_yet**, blocked on TLS existing on the client-facing surface at all, by whatever
mechanism.

### RQ4 — Secrets management — splits into an immediate fix and a deferred investment

Splits cleanly:
- **Immediate, zero-infrastructure fix**: rotate the leaked AWS key now, per OWASP's own
  revoke-first incident procedure. This is `build_now` in the trivial sense that it requires no
  code — only an IAM-console action — and remains open as of this report (see `AGENTS.md`'s
  Gotchas section and `SECURITY.md`'s own disclosure).
- **Genuine but currently disproportionate infrastructure investment**: Vault/AWS Secrets Manager
  dynamic, auto-expiring credentials. Real trigger: Kelvran's *next* new AWS credential, not
  today's single-pilot scale — introducing a secrets-manager dependency to fix one already-leaked
  key would be a disproportionate response to the actual problem size today.

### RQ5 — mTLS/zero-trust for the admin surface — genuinely open

Zero surviving claims this round despite being asked directly. `THREAT_MODEL.md`'s own
Elevation-of-Privilege row already treats a compromised admin bearer credential as full
control-plane privilege escalation — mTLS would add a second factor beyond the bearer token, but no
evidence either confirms or refutes this as the right next step versus other admin-hardening
options.

**Verdict:** genuinely unresolved — disclosed as an open gap, not silently dropped.

## Sources

- https://pkg.go.dev/net/http#Transport
- `gateway/cmd/gateway/main.go` (`newUpstreamTransport`, `upstreamMaxIdleConnsPerHost`)
- `gateway/cmd/gateway/upstream_transport_test.go`
- `docs/upgrade-research/performance-latency-optimization-2026-09-14.md` (sibling report, same day)
- `gateway/internal/router/router.go`, `docs/rfcs/2026-09-04-weighted-routing.md`
- `THREAT_MODEL.md` (Gateway DoS, Elevation of Privilege rows), `AGENTS.md` (Gotchas section)
