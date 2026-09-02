# Branch Strategy

> **Reopened and reaffirmed 2026-09-02.** This decision was revisited via a dedicated end-to-end research pass explicitly weighing GitFlow (main/develop/feature/release/hotfix), trunk-based development/GitHub Flow, and hybrid/multi-component-monorepo precedent against Kelvran's specific shape — not assumed by default. The founder's own suggested main(release)+develop(integration)+feature-via-develop model was seriously considered and explicitly **not adopted**; see "Why Not GitFlow" below for why. Full research trail: `Not-Humans-World/ai-infra-research/branch-strategy-trunk-based.md` and `branch-strategy-hybrid-models.md` (parent workspace).

## Branches

Exactly one long-lived branch: **`main`**. No `develop`. No per-deployable long-lived branch. No environment branches. Feature/fix work happens on short-lived `feat/description` or `fix/description` branches, per `CONTRIBUTING.md`'s existing convention — this document doesn't change that, it fills in the mechanics `CONTRIBUTING.md` leaves open (tagging, per-deployable releases, contract-changing branches, hotfixes).

- Branch from `main`, PR back into `main` directly.
- Kept short-lived — days, not weeks.
- Conventional commit format on merge: `<type>(<scope>): <description>`, scope generally `gateway`, `evals`, `cache`, `api`, or a specific subsystem.

## Why Not GitFlow

GitFlow's own author scopes its applicability, in his own 2020 retraction note, to software with multiple concurrently-supported version lines in the wild — not to "we version our releases independently," which Kelvran already does per-deployable without needing GitFlow for it (see `RELEASE.md`; independent versioning is a **tagging concern**, not a **branch-topology concern**). Of the real OSS infra projects checked directly against their own branch/release docs during this research — Kubernetes, Envoy, PostgreSQL, Redis, Kong, LiteLLM, Temporal, vLLM — six of seven run trunk-based-with-release-branches and zero run a permanent `develop`. The one partial counter-example, LiteLLM, is best explained by very high external-contributor PR volume rather than by any versioning need, and its own live branch state at research time showed a 91-commit divergence between its staging branch and `main` — the exact drift failure GitFlow's critics predict, observed in practice, in a team with far more headcount than Kelvran has now or will have in 1-2 years.

Structurally, the only way to preserve GitFlow's isolation guarantees across Kelvran's two independently-versioned deployables would be to duplicate its entire branch set per component — doubling process weight today and scaling linearly with every future component. That's the same "premature complexity gets adopted too early and has to be walked back" pattern this project's own team-trajectory research already found in Segment's and InVision's service-boundary reversals (`ai-infra-research/decision-single-vs-separate.md`), recurring one layer down in branch topology instead of service topology.

## Why Not Pure, Unmodified Trunk-Based Either

The strictest form of trunk-based development (tag directly off `main`, never branch, not even temporarily — the Google/Potvin variant) works cleanly for a single-artifact repo, which is what Kubernetes/Envoy/Redis actually are. Kelvran is not: it has two independently-tagged deployables sharing one `main`, so a `gateway` hotfix and in-flight, unreleased `evals` work (or vice versa) can legitimately coexist on `main` at the same moment — tagging straight off current `main` in that situation would ship the unrelated in-flight work bundled with the urgent fix. That's exactly the gap the hotfix escape valve below closes, which is why this document specifies real discipline for that branch rather than leaving it as unstructured "polish."

## Tagging Mechanics (Normal Release — unchanged)

Both `gateway` and `evals` release independently (see `RELEASE.md`), and both tag from `main`:

1. Merge the PR that moves `<deployable>/changelog/unreleased.md`'s content into a new, dated `<deployable>/changelog/<version>.md`; reset `unreleased.md` to empty category headers.
2. Tag the resulting `main` commit `gateway/v<version>` or `evals/v<version>` (SemVer).
3. Build/publish per `RELEASE.md`'s publish-targets table.
4. Verify the artifact actually resolves from the real registry before calling the release done.

## Hotfix / Escape Valve — Release Flow / Branch-for-Release Discipline

Previously this document named the `release/<deployable>-vX.Y` branch as an escape valve without specifying its internal discipline. It now follows **Release Flow / Branch-for-Release** exactly, scoped per deployable (a synthesis this document makes deliberately — no single surveyed precedent applies this pattern to more than one independently-versioned artifact per repo, because most sources describing it assume a single-artifact repo):

1. **Only used when** `main` has moved on since a deployable's last tag *and* a fix can't wait for that deployable's next full release. Rare by design — the default path is always the normal release cut above.
2. **The fix lands on `main` first**, as a normal `fix/*` branch, normal PR, normal review, normal merge. Never authored directly on a release branch.
3. **Cut `release/<deployable>-vX.Y` just-in-time**, from the last tag on that line — not proactively maintained, not pre-created "just in case."
4. **Cherry-pick the already-merged `main` commit(s) forward** onto `release/<deployable>-vX.Y`. Never the reverse.
5. **Tag the resulting commit** `<deployable>/v<patched-version>` (e.g. `gateway/v0.1.1`) directly on that branch.
6. **Never merge the branch back into `main`** — `main` already has the fix from step 2. Delete the branch once superseded; history survives via the tag, not the branch.

## `api/` Contract-Touching Branches

Any branch that changes a file under `api/` must declare, in its PR description, whether the change is breaking or non-breaking per `buf breaking`'s categories. Per `AGENTS.md`'s existing "ask first" rule for `api/` changes, open the conversation before writing the branch, not after. See `RELEASE.md`'s "Contract-Version Bump-and-Validate Procedure" for what has to happen before either deployable can ship against a changed contract.

## Verification Discipline After Tagging

A tag is not a release. After tagging, confirm the artifact actually resolves from the real registry (Go module proxy for `gateway`, PyPI for `evals`) before calling the release done — a tag that never got picked up by the module proxy, or a PyPI upload that silently failed, is a real failure mode worth checking for explicitly rather than assuming success from a green CI run.

## Revisit Triggers (two distinct triggers — do not conflate)

**(a) Organizational trigger** — unchanged from ADR-0001's own pattern. Revisit whether an integration branch (MindForge's own `develop → release → main` model, or a variant) is worth adopting the moment **a distinct team, not a distinct engineer, owns `gateway` or `evals` end-to-end with independent release cadence and on-call**.

**(b) Support-policy trigger** — new, from this research round. Graduate a deployable's short-lived, deleted-after-use escape valve into a long-lived, cherry-pick-only maintenance branch per version line (Kubernetes `release-X.Y` / PostgreSQL `REL_X_STABLE` style) the moment Kelvran **commits, in writing in `RELEASE.md` or `UPGRADE.md`, to concurrently supporting more than one live major version of `gateway` or `evals` in production**.

**Explicitly not a trigger for either:** team size alone, PR volume alone, or "it would be cleaner."

## Migration Note

No branch beyond `main` was required at git-init time. The first commit(s) — the already-verified initial scaffolding — go directly to `main`. `release/<deployable>-vX.Y` branches are created lazily, only when an actual post-tag hotfix is needed.
