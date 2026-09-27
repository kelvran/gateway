# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

## Changed

## Deprecated

## Removed

## Fixed

- The streaming near-duplicate-collision counter (`kelvran.streaming.near_duplicate_collision`) fired on concurrent identical requests that hit an already-warm L1/L2/L3 cache — a false signal, since a cache hit never reaches upstream regardless of this counter, directly undermining the metric's own purpose of measuring genuinely upstream-bound duplication risk. Moved the observation point to after both cache checks miss, mirroring the buffered path's own `runMissPath` scoping.
- `-validate` never actually caught a malformed `allowed_source_cidrs` CIDR string — it only calls `controlplane.Load`, never `cmd/gateway.buildPipeline` (where the real syntax check lived), so a config that would crash the process on real startup could still report "config is valid". `Load` now performs the same `net.ParseCIDR` check itself.

## Security

- A virtual key's `allowed_source_cidrs`, `allowed_models`, or `allowed_regions` restriction was silently, completely disabled if authored as a YAML list (e.g. `allowed_source_cidrs:\n  - "::1/128"`) instead of the required mapping-of-`true` shape — this parser has no YAML list support at all, and the list-shaped line garbled into a value that failed a type check and was silently dropped, with zero error at load, validate, or (for `allowed_source_cidrs`) even real startup time. `Load` now rejects any non-boolean value under these three fields with a loud, named error. Found via a live end-to-end dry run against a real running gateway process — a real IPv4 request to a key restricted to `::1/128` returned `200` instead of the expected `403`. If your own config sets any of these three fields, run `gateway -validate` once against it to confirm it wasn't silently disabled by this shape.
- `resolveClientIP` now fails closed (denies) on an ambiguous, unbracketed IPv6-with-port `RemoteAddr` string (e.g. `"::1:1234"`) instead of passing it through to be misparsed by `net.ParseIP` as a different, valid-looking IPv6 address and checked against the CIDR allowlist under that substituted identity. A real `net/http` `RemoteAddr` for IPv6 is always bracketed, so this never fires against real production traffic — it only closes a theoretical substitution risk for a malformed/hand-constructed input.
