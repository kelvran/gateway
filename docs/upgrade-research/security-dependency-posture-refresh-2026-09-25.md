# Kelvran Security & Dependency Posture Refresh — 2026-09-25

**Date:** 2026-09-25
**Scope:** New CVE/GHSA disclosures against Kelvran's *actual* dependency tree (`gateway/go.mod`+`gateway/go.sum` — Go; `evals/pyproject.toml` — Python) and new OWASP LLM Top 10 revisions / supply-chain-security tooling advances (SBOM, Sigstore/cosign, container/base-image scanning, GitHub Actions hardening) since this repo's own CI security tooling was last set up. Explicitly scoped to *new, actionable* findings only — every candidate was checked against `SECURITY.md`, `THREAT_MODEL.md`'s NIST/OWASP crosswalk sections, and `.github/workflows/ci.yml`'s real, current jobs (`govulncheck`, `pip-audit`, CodeQL, `gosec` via `golangci-lint`, cosign/SBOM/SLSA-provenance attestation, Dependabot, OpenSSF Scorecard, Checkov, kubeconform) before being written up here, and dropped or reframed as "checked, not applicable" where it duplicated existing coverage.
**Method:** Adversarial multi-source research (3-vote verification per claim) against official sources only — the GitHub Advisory Database, NVD, MITRE CVE.org, each project's own published security advisories, and OWASP's own published Top 10 revision — never a secondary blog post as the sole source. 21 claims survived 3-vote adversarial verification (4 more were explicitly refuted, 0-3/1-2 splits, and correctly excluded). This document adds the Kelvran-specific grounding layer the raw adversarial verification deliberately does not do: a live check of every candidate CVE/GHSA's affected-version range against Kelvran's own pinned versions in `gateway/go.sum`/`evals/pyproject.toml`, a grep for whether the vulnerable feature/package is even reachable from Kelvran's code at all, and a cross-reference against `THREAT_MODEL.md`'s existing OWASP/NIST crosswalk tables and `ci.yml`'s existing jobs to avoid re-reporting something already covered.

---

## Executive summary

This refresh found **zero live, actionable vulnerabilities** in Kelvran's actual dependency tree as pinned today, and confirms `THREAT_MODEL.md`'s OWASP LLM Top 10 crosswalk is already current against the newest (2026-08-04) OWASP GenAI Security Project edition — including the Excessive Agency rank-6→3 move, which this repo already re-keyed on 2026-09-20 (and first flagged 2026-09-10). Six new 2026 CVEs/GHSAs were checked against Kelvran's real pins and none apply: two AWS SDK for Go v2 advisories (an EventStream-decoder DoS and a region-validation defense-in-depth notice) sit below versions Kelvran already exceeds or touch subpackages Kelvran never imports; a go-redis/bbolt sweep found no new disclosures and one advisory that was itself withdrawn as a false positive; two `anthropic` PyPI SDK CVEs (a TOCTOU symlink race and world-writable memory files in the async local-filesystem memory tool) sit in a version range `evals/pyproject.toml`'s `anthropic>=1.3.0` pin is already well past, and a repo grep confirms the memory-tool feature itself is never called; and a `pydantic-settings` symlink-traversal advisory, while real and accurately described, doesn't touch Kelvran at all — the package isn't a dependency, direct or transitive. On the tooling side, cosign shipped both a High-severity `verify-blob`/`verify-blob-attestation` signature-bypass fix (GHSA-fx35-mq7g-6g98, v3.1.3/v2.6.5, Aug 6 2026) and a Rekor v2/bundle-format signing-path migration (v3.1.1, Jun 9 2026) since this repo's cosign step was first wired in — neither is a live gap: `RELEASE.md`'s own consumer-facing verification commands use `cosign verify`/`cosign verify-attestation` (container-image verification), not the affected `verify-blob` family, and `ci.yml`'s `cosign-installer` step carries no version pin, so it already installs whatever cosign is current at each run. The net finding is a clean bill of health this cycle, not an absence of research — every item above required checking a real version/feature boundary, not assuming safety from a claim's severity rating alone.

---

## Findings, ranked

### 1. OWASP GenAI LLM Top 10 2026 — `THREAT_MODEL.md`'s crosswalk is already current (Informational — confirms no drift)

**What:** OWASP's GenAI Security Project published a new "Top 10 for LLM Applications 2026" edition (2026-08-04), with 8 of 10 category IDs changed, one category renamed/re-scoped (System Prompt Leakage → Hidden Context Exposure), two 2025 categories restored (Supply Chain; Improper Output Handling), Excessive Agency moving rank 6→3, and new framework mappings to NIST, MITRE ATLAS, CWE, plus a standalone "GenAI Security Industry Framework Crosswalk" resource and a "Top 10 for Agentic Applications" companion document.

**Why it matters for Kelvran specifically:** the research brief asked whether `THREAT_MODEL.md`'s NIST/OWASP crosswalk is grounded against the *current* OWASP mapping set. It is. `THREAT_MODEL.md`'s own "Corrected 2026-09-20" note already independently re-verified this exact edition (cites the same "OWASP Top 10 for LLM Applications 2026, v2026, 2026-08-04" PDF) and re-keyed every row to the real 2026 IDs — including naming the Excessive Agency 6→3 move as "the most consequential move in the 2026 edition," first flagged even earlier (`DECISIONS.md`, 2026-09-10). This is a genuine, already-closed item, not a gap this research reopens.

**Already covered by existing tooling/docs?** Yes — `THREAT_MODEL.md` §"OWASP LLM Top 10 (2026) Crosswalk" (lines 49-66) and its 2026-09-20 change-log entry. No new row or correction is needed from this research pass.

**2026 best practice grounding:**
- OWASP's own official page confirms the 2026 edition "introduces updated rankings, expanded threat coverage, and new research... while mapping risks to leading industry frameworks including NIST, MITRE ATLAS, CWE, and the OWASP Top 10 for Agentic Applications" [genai.owasp.org/resource/owasp-genai-llm-top-10-2026/ — confirmed 3-0, verified via direct HTTP fetch, dated 2026-08-04].
- The Excessive Agency rank-3 move is independently confirmed both by a sponsor quote in OWASP's own release announcement and, more strongly, by the primary Top 10 2026 PDF itself (LLM03:2026, page 23; prose: "Excessive Agency climbed to third, the most consequential move on the list") [genai.owasp.org — confirmed 2-1, high-confidence on the primary-document cross-check].
- A new, standalone "GenAI Security Industry Framework Crosswalk" resource now exists on OWASP's own domain, distinct from the Top 10 document itself, explicitly built to "help organizations connect OWASP GenAI security guidance with established security, risk and compliance frameworks" [genai.owasp.org — confirmed 3-0].

**Concrete next step:** none required for the OWASP crosswalk itself. The one genuinely open piece — whether OWASP's new standalone Framework Crosswalk resource offers a finer-grained mapping than `THREAT_MODEL.md`'s separately-hand-maintained NIST AI 600-1 table — is carried into Open Questions below rather than asserted as a gap here, since no claim in this research round actually compared the two documents' content side by side.

**Effort:** None — no doc or code change needed this cycle.

---

### 2. AWS SDK for Go v2 — two 2026 advisories, neither reaches Kelvran's pinned versions (Checked, not applicable)

**What:** Two GitHub Security Advisories against `aws/aws-sdk-go-v2` disclosed in 2026: GHSA-xmrv-pmrh-hhx2 / CVE-2026-89090, a Moderate (CVSS 3.1, 5.9) denial-of-service in the shared `aws/protocol/eventstream` header decoder (a malformed frame with an out-of-range header-value type byte can crash the host process), fixed at `eventstream` v1.7.8 and cascading into 11 dependent service-client packages (S3, Bedrock Runtime, Lambda, Kinesis, etc.) each with their own minimum patched version; and GHSA-3jcv-796g-cpjg, a Low-severity "defense in depth" advisory (published 2026-01-09) hardening region-string validation before endpoint-URL construction, affecting 406 `aws-sdk-go-v2/service/*` subpackages.

**Why it matters for Kelvran specifically:** `gateway/go.mod`/`go.sum` pin `github.com/aws/aws-sdk-go-v2 v1.47.0` (core) and `github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.20` directly — confirmed by reading the files, not inferred. `v1.7.20` is already well past the `< v1.7.8` vulnerable range for CVE-2026-89090, so the EventStream DoS does not apply. A repo-wide grep found **zero** occurrences of any `aws-sdk-go-v2/service/*` subpackage anywhere in `go.mod`/`go.sum` (Kelvran hand-builds its Bedrock Converse wire format instead of depending on a typed SDK service client — confirmed by a code comment in `bedrock.go` stating exactly this), and the region-validation advisory's full 406-entry affected-package list contains no core module, no `eventstream`, and no non-`service/*` entry at all — so GHSA-3jcv-796g-cpjg's affected-package set has zero intersection with Kelvran's actual AWS SDK footprint either.

**Already covered by existing tooling?** This is exactly the class of finding `govulncheck` (wired into the `gateway` CI job) is designed to catch on every push — both advisories are checked-clear today given the current pin, and `govulncheck` will re-flag automatically if a future dependency bump ever regresses `eventstream` below v1.7.8.

**2026 best practice grounding:**
- GHSA-xmrv-pmrh-hhx2 confirmed against three independent official sources in full agreement (GitHub Advisory Database API, NVD REST API, MITRE CVE.org record) — CVSS 3.1 5.9, CWE-20, published 2026-09-11, last updated 2026-09-24 [github.com/aws/aws-sdk-go-v2/security/advisories/GHSA-xmrv-pmrh-hhx2 — confirmed 3-0].
- The fix and its 11-package cascade confirmed directly against the advisory's own structured API record (`gh api /advisories/GHSA-xmrv-pmrh-hhx2`), not the summarized webpage, after an earlier fetch of the page itself produced internally-inconsistent secondary details [github.com/aws/aws-sdk-go-v2/security/advisories/GHSA-xmrv-pmrh-hhx2 — confirmed 3-0].
- GHSA-3jcv-796g-cpjg's title, Low severity, 2026-01-09 publish date, and its full 406-row affected-package table (100% `service/*` subpackages, zero core-module or `eventstream` rows) confirmed directly against the GitHub Security Advisory API for the advisory itself [github.com/aws/aws-sdk-go-v2/security/advisories/GHSA-3jcv-796g-cpjg — confirmed 3-0].

**Concrete next step:** none. Record in this refresh (and, if desired, `THREAT_MODEL.md`'s Review Cadence log) that both advisories were checked against the live pin and found not applicable, so a future reviewer doesn't have to re-derive the same conclusion from scratch on the next CVE sweep.

**Effort:** None.

---

### 3. go-redis and bbolt — no new disclosures; one advisory was itself withdrawn (Checked, not applicable)

**What:** `redis/go-redis`'s entire GitHub Security Advisories page lists exactly one advisory total, as of 2026-09-25 — GHSA-92cp-5422-2mw7 / CVE-2025-29923 (a `CLIENT SETINFO` connection-establishment timeout ordering bug), published 2025-03-20, with no 2026 or late-2025 entries at all. Separately, `go.etcd.io/bbolt`'s GHSA-6jwv-w5xf-7j27 / CVE-2026-33817 (an "index out of range" report) was formally withdrawn by its own CVE Numbering Authority as a false positive.

**Why it matters for Kelvran specifically:** `gateway/go.mod` pins `github.com/redis/go-redis/v9 v9.22.0` and `go.etcd.io/bbolt v1.5.0` — confirmed by reading the files. GHSA-92cp-5422-2mw7's fixed versions are 9.5.5/9.6.3/9.7.3, all far below Kelvran's v9.22.0 pin, so it isn't even a live exposure independent of its age; there is simply no newer go-redis advisory to check against. The bbolt advisory needs no version check at all — its own CNA declared it a false positive, so it never described a real vulnerability in any version.

**Already covered by existing tooling?** Yes, in the sense that `govulncheck` already tracks both modules on every CI run and would surface a real new disclosure automatically; this pass exists to answer whether one has appeared since the tooling was set up, and confirms none has.

**2026 best practice grounding:**
- go-redis single-advisory-page finding independently re-verified via four channels in full agreement: the GitHub REST API for the repo's own security advisories, a live fetch of the advisories page, GitHub's global Advisory Database filtered to `github.com/redis/go-redis/v9`, and OSV.dev's own vuln database for the same package [github.com/redis/go-redis/security/advisories — confirmed 3-0].
- The bbolt withdrawal is stated verbatim in the advisory's own record: "This advisory has been withdrawn because its CVE Numbering Authority has determined this issue to be a false positive," `withdrawn_at: 2026-04-13` [github.com/etcd-io/bbolt/security/advisories/GHSA-6jwv-w5xf-7j27 — confirmed 3-0, verified against the GitHub Advisory Database API directly after the HTML advisory page itself 404'd].

**Concrete next step:** none.

**Effort:** None.

---

### 4. Anthropic Python SDK — two 2026 memory-tool CVEs, version and feature both out of reach (Checked, not applicable)

**What:** Two GitHub-reviewed advisories against the `anthropic` PyPI package (Claude SDK for Python), both scoped to versions `>=0.86.0, <0.87.0` and fixed in `0.87.0`: GHSA-w828-4qhx-vxx3 / CVE-2026-34452, a TOCTOU symlink race in the async local-filesystem memory tool (a path is validated as staying inside the sandboxed memory directory, but the *unresolved* path is then used for the actual file operation, so a local attacker who can write into that directory can retarget a symlink between check and use to escape the sandbox); and GHSA-q5f5-3gjm-7mfm / CVE-2026-34450, the same memory tool creating memory files with mode `0o666` — world-readable by default, world-writable under a permissive umask (the advisory names "many Docker base images" as a concrete example environment).

**Why it matters for Kelvran specifically:** `evals/pyproject.toml` pins `anthropic>=1.3.0` — confirmed by reading the file — well above the `0.87.0` fixed version, let alone the `>=0.86.0, <0.87.0` vulnerable range, so neither CVE's version precondition is met. Independently, a repo-wide grep for the memory-tool's own API surface (`memory_20`, `beta_builtin_memory`, `BetaMemoryTool`, `local_filesystem`) across `evals/` returned zero matches — `anthropic` is used only in `evals/evals/cli.py` and `evals/evals/judge/providers.py` (as an LLM-judge client), never via the memory-tool feature these two CVEs are scoped to. Both the version gate and the feature-usage gate independently rule this out.

**Already covered by existing tooling?** `pip-audit` (wired into the `evals` CI job) is the mechanism that would catch either CVE automatically if `evals/pyproject.toml`'s pin ever regressed below `0.87.0`; this pass confirms today's pin already clears that bar with substantial margin.

**2026 best practice grounding:**
- GHSA-w828-4qhx-vxx3 / CVE-2026-34452 confirmed via direct GitHub Advisory API call plus the source repo's own published advisory, cross-checked against the actual fix commit (`6599043e`, `_validate_path` changed to return a resolved path) and its accompanying symlink-swap regression test [github.com/advisories/GHSA-w828-4qhx-vxx3 — confirmed 3-0]. CWE-59/CWE-367, CVSS v3 5.3 (Moderate), local-attacker precondition.
- GHSA-q5f5-3gjm-7mfm / CVE-2026-34450 independently confirmed against both the GitHub Advisory Database and the public NVD REST API, with identical description text in both [github.com/advisories/GHSA-q5f5-3gjm-7mfm — confirmed 2-1, corroborated by an independent NVD record]. CWE-276/CWE-732, CVSS v3.1 4.4 (Medium).
- The exact affected/patched version range (`>=0.86.0, <0.87.0` → `0.87.0`) is the advisory's own canonical, GitHub-reviewed metadata for both CVEs, cross-checked against the release's own changelog entries [github.com/advisories/GHSA-q5f5-3gjm-7mfm — confirmed 2-1].

**Concrete next step:** none required now. If `evals`' rollout/sandbox code ever adopts the async local-filesystem memory tool in the future, re-check this CVE class at that time rather than assuming today's "not applicable" verdict still holds — carried into Open Questions below.

**Effort:** None.

---

### 5. pydantic-settings symlink-traversal advisory — real, but not a Kelvran dependency at all (Checked, not applicable; split votes on applicability framing)

**What:** GHSA-4xgf-cpjx-pc3j, a Moderate-severity (CVSS 5.3), no-CVE-assigned advisory against `pydantic-settings` (versions `>=2.12.0, <2.14.2`, fixed in `2.14.2`, published 2026-06-19): `NestedSecretsSettingsSource` used two inconsistent directory-traversal methods — its size-check pass (`Path.glob('**/*')`) does not follow symlinked directories, but its actual secret-loading pass (`glob.iglob(..., recursive=True)`) does — so a symlinked directory entry pointing outside the configured `secrets_dir` gets read into settings values, and the documented `secrets_dir_max_size` size cap is silently bypassed in the process.

**Why it matters for Kelvran specifically:** it doesn't, directly. A repo-wide search (`evals/pyproject.toml`, `evals/uv.lock`, and a full-text grep) found zero references to `pydantic-settings` anywhere — `evals` depends on `pydantic>=2.0` directly but never on `pydantic-settings`, confirmed by reading `pyproject.toml`. This advisory cannot affect Kelvran's dependency tree because the package isn't in it, direct or transitive.

**Already covered by existing tooling?** N/A — there is nothing for `pip-audit` to ever flag here, since the package is absent.

**2026 best practice grounding:**
- The root-cause mechanism (inconsistent `Path.glob` vs. `glob.iglob(recursive=True)` symlink-following behavior, and the resulting size-cap bypass) is confirmed verbatim, word-for-word, against the advisory's own official GitHub Advisory Database record — not a secondary interpretation [github.com/advisories/GHSA-4xgf-cpjx-pc3j — confirmed 3-0].
- Two related claims characterizing the advisory's *applicability* were explicitly refuted during adversarial voting (0-3 and 1-2) — the underlying technical mechanism above survived verification, but broader claims framing this as something Kelvran needs to act on did not, consistent with the "not a dependency" finding above.

**Concrete next step:** none for Kelvran's dependency tree today. The underlying traversal-inconsistency *pattern* (a size/validation check using a symlink-unaware directory walk, paired with a symlink-following load path) is a generic secrets-loading anti-pattern worth a quick sanity check against Kelvran's own config-loading code independent of this specific third-party package — carried into Open Questions below, since no claim in this round actually audited Kelvran's own code for the pattern.

**Effort:** None for the dependency question; a Small, separate self-audit if the Open Question above is pursued.

---

### 6. Sigstore cosign — a High-severity `verify-blob` bypass and a Rekor v2/bundle-format migration, neither a live gap for Kelvran (Informational)

**What:** Two cosign advances since `ci.yml`'s `publish-image` job first wired in cosign signing/SBOM/SLSA-provenance attestation: GHSA-fx35-mq7g-6g98, a High-severity (CVSS 3.1, 7.4) verification-bypass vulnerability, where keyless identity/issuer checks (`--certificate-identity`, `--certificate-oidc-issuer`) could be bypassed specifically in `cosign verify-blob`/`cosign verify-blob-attestation` when a legacy JSON `--bundle` format was supplied — fixed in cosign v3.1.3 (v2 line backported to v2.6.5), both released 2026-08-06; and a separate, non-vulnerability supply-chain advance, cosign v3.1.1 (2026-06-09) moving its default signing path to Rekor v2 (DSSE attestations logged as hashed entries using pre-authentication encoding) and standardizing the "bundle" format as the default sign/verify output/input across all Sigstore SDKs.

**Why it matters for Kelvran specifically:** neither reaches Kelvran's actual flow. GHSA-fx35-mq7g-6g98's own advisory explicitly scopes impact to `cosign verify-blob`/`cosign verify-blob-attestation` only — not `cosign verify`/`cosign verify-attestation`, and not the new bundle format. `RELEASE.md`'s own consumer-facing verification instructions (the commands an operator is told to run against the published `ghcr.io/kelvran/gateway` image) use exactly `cosign verify` and `cosign verify-attestation` — container-image verification, a different code path from the one this advisory affects — confirmed by reading `RELEASE.md` directly. Separately, `ci.yml`'s `Install cosign` step (`sigstore/cosign-installer@...#v4.1.2`) carries no `cosign-release` input — confirmed by reading the workflow file — so it installs whatever cosign release is current at each CI run, meaning it has already been pulling a patched (≥v3.1.3) cosign binary automatically since shortly after the Aug 6 2026 fix shipped, with no action needed.

**Already covered by existing tooling?** Functionally yes, by the unpinned installer's own floating-latest behavior — though that is an emergent property of an absence of a version pin, not a deliberate control anyone verified before now.

**2026 best practice grounding:**
- GHSA-fx35-mq7g-6g98 confirmed directly against cosign's own release notes and its GitHub Security Advisory record via authenticated `gh api` calls (not the summarized webpage), including cross-checking that v2.6.5 and v3.1.3 exist as real, sequential release tags and that the patch release preceded the advisory's own `published_at` by about 21 minutes — a realistic disclosure sequence [github.com/sigstore/cosign/security/advisories/GHSA-fx35-mq7g-6g98 — confirmed 3-0]. CWE-295/CWE-347.
- The Rekor v2/bundle-format migration confirmed directly against cosign v3.1.1's own release body text on GitHub, cross-checked against `sigstore/rekor-tiles`' own release history for corroborating timing [github.com/sigstore/cosign/releases — confirmed 2-1, high-confidence on the primary-source release text].
- A related claim that the same bypass was "also backported to v2.6.x... indicating it affects both the legacy and current bundle-format code paths" was explicitly refuted (1-2) — the backport is real, but the inference about *why* (both code paths affected) did not survive verification; the advisory's own scoping to `verify-blob`/`verify-blob-attestation` only, cited above, is the safer read.

**Concrete next step:** optional, not urgent — pin `cosign-installer`'s `cosign-release` input explicitly (e.g., to a version confirmed ≥v3.1.3) for build reproducibility/auditability, matching this repo's own stated preference for pinned, non-floating CI dependencies elsewhere in `ci.yml`. This is a hygiene improvement, not a vulnerability remediation — the installer already floats to a patched version today.

**Effort:** Small, if pursued — a one-line `with: cosign-release: "vX.Y.Z"` addition, plus a periodic bump alongside Dependabot's existing `github-actions` ecosystem cadence.

---

## Top 3 do next

1. **Record this refresh's "checked, not applicable" verdicts somewhere durable** (Small) — `THREAT_MODEL.md`'s Review Cadence & Change Log, or `DECISIONS.md`, per this repo's own established convention for research passes that conclude "no action needed." Six specific CVE/GHSA IDs (GHSA-xmrv-pmrh-hhx2, GHSA-3jcv-796g-cpjg, GHSA-92cp-5422-2mw7, GHSA-6jwv-w5xf-7j27, GHSA-w828-4qhx-vxx3, GHSA-q5f5-3gjm-7mfm) and one supply-chain advisory (GHSA-fx35-mq7g-6g98) were each individually checked against Kelvran's real pins/flow and found not applicable — writing that down once means the next CVE sweep doesn't have to re-derive the same six-plus-one conclusions from scratch.

2. **Optionally pin `cosign-installer`'s `cosign-release` input** (Small) — the only concrete, if non-urgent, CI hygiene gap surfaced this cycle. Not a vulnerability fix (the unpinned installer already floats to a patched cosign), but consistent with this repo's own general preference for pinned, reproducible CI dependency versions.

3. **No dependency version bumps are required this cycle.** Every Go and Python pin checked (`aws-sdk-go-v2`+`eventstream`, `go-redis`, `bbolt`, `anthropic`) already sits above every disclosed vulnerable range found, and `pydantic-settings` was never a dependency to begin with. `govulncheck`/`pip-audit`/Dependabot remain the correct standing mechanism to catch the *next* disclosure — this refresh doesn't replace them, it's a point-in-time confirmation they haven't missed anything for these six.

---

## Caveats

- **This is a clean-bill-of-health result, not a light research pass.** Every "not applicable" verdict above required an actual version-range comparison against `go.sum`/`pyproject.toml`, and in two cases (the anthropic memory-tool CVEs, the pydantic-settings advisory) an additional grep for whether the vulnerable feature/package is even reachable from Kelvran's code — not an assumption from a claim's severity rating or an advisory's mere existence.
- **Split/non-unanimous votes**, all still rated high-confidence on primary-source grounds but flagged for transparency: the Excessive-Agency-rank-3 primary-document cross-check (2-1); the OWASP 2026 edition's "updated rankings" characterization (2-1); the OWASP Framework Crosswalk mapping claim (2-1, though independently corroborated by asset-path evidence on OWASP's own domain); the "distinct 2026 edition exists" claim (2-1); GHSA-q5f5-3gjm-7mfm's TOCTOU-adjacent characterization and version-range claim (2-1 each); cosign's Rekor v2/bundle-format migration claim (2-1).
- **Four claims were explicitly refuted** during adversarial verification and are not carried into any finding above as confirmed: two pydantic-settings *applicability* framings (0-3, 1-2 — the underlying technical mechanism in Finding 5 still stands on its own 3-0 evidence); a claim that OWASP's 2026 Top 10 document carries an explicit "v1.0" version label distinct from the 2025 edition (0-3); and a claim that cosign's v2.6.x backport implies both legacy and current bundle-format code paths are affected by GHSA-fx35-mq7g-6g98 (1-2 — the advisory's own narrower `verify-blob`-only scoping, cited in Finding 6, is the safer read).
- **Source-reliability note carried over from the underlying research**: several verifiers explicitly worked around a known sandboxed-WebFetch content-mocking risk by cross-checking primary sources via direct, authenticated API calls (`gh api`, raw `curl` to `api.github.com`/NVD/OSV.dev) rather than trusting a single WebFetch summarization — this is why several findings above cite "API," not "page," verification. No finding in this document rests on a single unverified WebFetch call.
- **Time-sensitivity**: OWASP's 2026 Top 10 edition is ~7 weeks old relative to this research date and, per its own site structure, had not yet been folded into OWASP's interactive category-browsing taxonomy pages at fetch time (only the standalone whitepaper/resource page existed) — a minor OWASP-side publishing-lag caveat, not a reason to doubt the document's content. Cosign's GHSA-fx35-mq7g-6g98 fix (Aug 6 2026) and Rekor v2 migration (Jun 9 2026) are both within the last ~4 months and unlikely to have been reversed by a later release.
- **Scope boundary**: this refresh covers only Go module dependencies in `gateway/go.mod`/`go.sum` and Python dependencies in `evals/pyproject.toml`, per the research brief. Docker base-image CVEs are already covered by `ci.yml`'s Trivy scan of the published image, and Terraform/Kubernetes manifest security is already covered by `iac-scan.yml`'s Checkov/kubeconform jobs — neither was re-litigated here.

## Open questions

- Does OWASP's new standalone "GenAI Security Industry Framework Crosswalk" resource (distinct from the Top 10 2026 document itself) offer a finer-grained NIST AI 600-1/MITRE ATLAS/CWE mapping than `THREAT_MODEL.md`'s own separately hand-maintained NIST crosswalk table — worth a dedicated side-by-side comparison pass, since no claim in this round actually read the two documents against each other?
- Is Kelvran's own config/secrets-loading code (`gateway/internal/gateway/controlplane/config.go` or equivalent) free of the specific traversal-inconsistency pattern behind GHSA-4xgf-cpjx-pc3j (a size/validation check using a symlink-unaware directory walk, paired with a symlink-following load path) — independent of the fact that `pydantic-settings` itself isn't a dependency? The pattern itself is generic enough to be worth a quick, targeted self-audit.
- Should `evals/pyproject.toml`'s currently-unpinned-upper-bound `anthropic>=1.3.0` dependency carry an explicit note (in `THREAT_MODEL.md`'s Evals row, or a code comment near the SDK's usage) flagging that the async local-filesystem memory-tool CVE class (GHSA-w828-4qhx-vxx3/GHSA-q5f5-3gjm-7mfm) needs re-checking if `evals` ever adopts that specific SDK feature in the future, rather than relying on a future reader re-discovering "it's not currently used" from scratch?
- Is pinning `cosign-installer`'s `cosign-release` input (Finding 6's optional next step) worth the added Dependabot-tracked maintenance surface, given the installer's current floating-latest behavior has not actually caused a problem — or is "float to latest, verify no advisory currently applies" an acceptable standing posture for a tool this repo re-runs on every push?
