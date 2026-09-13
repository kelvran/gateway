# Synthetic Eval-Case Generation and Automated Red-Teaming for Kelvran's Regression Corpus (2026-09-13)

**Date:** 2026-09-13
**Scope:** Current (2026) state of practice for synthetic eval-case generation and LLM-as-red-teamer self-play, assessed specifically against Kelvran's own regression corpus (`evals/tests/fixtures/regression_corpus_*.json` — 113 + 24 = 137 hand-authored cases across `cache_adversarial`/`cost_abuse`/`dogfood`/`guardrail`/`routing_chaos`/`judge_accuracy`, CI-gated via `evals report --fail-under` on a Wilson-lower-bound) and its `guardrail`/prompt-injection category in particular (24 hand-authored cases today; `THREAT_MODEL.md` names prompt injection OWASP LLM01 as the highest-priority threat class). `evals/evals/audit_corpus.py` already exists but only *audits* existing cases for design defects via an LLM judge pass — it is explicitly documented in its own module docstring as report-only, never wired to any `--fail-under`-style gate, and deliberately never calls the judging mechanism it audits, precisely to avoid identical blind spots. Nothing today *generates* new cases.
**Method:** Adversarial multi-source research (3-vote verification per claim) against peer-reviewed 2025-2026 papers (AutoRedTeamer/NeurIPS 2025, Safety Self-Play/ACL Findings 2026, a six-category automated red-teaming paper), OpenAI's own GPT-Red primary technical paper, promptfoo's official product documentation (the only production-grade off-the-shelf red-team tool researched), and Anthropic's 2022 human red-teaming dataset release as a manual-at-scale contrast point. 15 claims survived 3-vote verification; 10 were refuted and are excluded from the findings below except where the refutation itself is informative.

---

## Executive summary

Real, working synthetic adversarial-generation pipelines exist in 2026 — OpenAI's GPT-Red self-play system reportedly beats human red-teamers on a held-out indirect-prompt-injection benchmark, and academic self-play/lifelong-attack-integration systems (AutoRedTeamer, Safety Self-Play) demonstrate LLM-as-red-teamer techniques achieving embedding-space diversity close to human-curated benchmarks — but every credible production pipeline surveyed treats generator output as **untrusted until validated**, using a mix of programmatic and LLM-based legitimacy checks plus specific anti-gaming filters (reject trivial-transform wrapping, reject non-plausible-conversation shapes) before a generated case counts for anything. Promptfoo, the one commercial off-the-shelf red-team tool researched in depth, ships a real, productionized taxonomy of prompt-injection/guardrail-bypass plugins (indirect injection, ASCII smuggling, system-prompt override, context-compliance-attack, special-token injection) directly relevant to Kelvran's OWASP LLM01 gap, but its own documentation shows **no pre-use quality gate on generated test probes** — validation there is entirely on the target's *response*, and promotion into its regression suite is gated only by empirical attack success, not by any dedup/quality/mislabeling audit. This is the central risk for Kelvran: every researched precedent that validates synthetic cases before trusting them does so with infrastructure Kelvran does not yet have (a validation-threshold loop, a reward-gated attack library, a second LLM judge dedicated to the generated case itself, not the target's response to it) — building generation without first building that validation gate would let unvalidated synthetic cases enter the exact `--fail-under` Wilson-lower-bound gate that currently blocks real PRs on 137 cases whose ground truth is presently hand-verified.

---

## Findings, ranked

### 1. Automated LLM-as-red-teamer self-play is a real, working 2025-2026 technique, not speculative — but every credible implementation gates generated attacks behind validation before trusting them (High confidence)

**What:** Three independent 2025-2026 lines of work demonstrate genuine LLM-as-red-teamer self-play: AutoRedTeamer (NeurIPS 2025) runs a single agent that proposes new attack implementations and only admits them to a reusable attack library if they clear a ≥30% attack-success-rate validation threshold on a HarmBench subset, refining and re-testing otherwise. Safety Self-Play (SSP, ACL Findings 2026) uses one LLM trained via RL to act as both Attacker and Defender concurrently in a unified RL loop, eliminating dependence on a fixed external red-teamer or a static pre-collected adversarial dataset — this is the closest primary-source match to "LLM-as-red-teamer self-play" as a named technique. OpenAI's GPT-Red (2026) explicitly validates the legitimacy of *both* the attack and the environment it's placed in with a mix of programmatic and LLM-based checks, assigning reward 0 to invalid attacks — "this ensures that all attacks are legitimate and punishes the attacker for finding invalid attacks," not merely trusting whatever the generator proposes.

**Why it matters for Kelvran:** the technique class the research question asks about is not vaporware — self-play generation of new adversarial cases against a target is demonstrably real, current, and already applied by at least one frontier lab (OpenAI) to the same threat class Kelvran flags as highest-priority (see Finding 4). But the validation-before-trust pattern is universal across all three independent implementations, not an optional extra — no surveyed system feeds unvalidated self-play output directly into a scored/gated outcome.

**Confidence:** High — three independent peer-reviewed/primary-source implementations (NeurIPS 2025, ACL Findings 2026, OpenAI 2026), all unanimous 3-0 votes on the core mechanism claims.

**Sources:** AutoRedTeamer (NeurIPS 2025, arXiv:2503.17899); Safety Self-Play (ACL Findings 2026, aclanthology.org/2026.findings-acl.933); GPT-Red (OpenAI, cdn.openai.com/pdf/gpt-red-automated-red-teaming-via-self-play-at-scale.pdf).

**build_now / not_yet: not_yet.** Trigger: this becomes buildable for Kelvran only once a validation-gate mechanism (see Finding 3) exists first — the technique itself is proven, but adopting it without the gate inverts the field's own consensus ordering.

---

### 2. Synthetic generation without human curation can approach human-curated diversity, but the diversity metric itself is an imperfect, gameable proxy — treat "diverse-looking" as necessary, not sufficient (Medium confidence)

**What:** AutoRedTeamer's own reported numbers (average pairwise cosine similarity between embeddings, lower = more diverse) show AutoRedTeamer at 0.45, PAIR (a single-technique automated jailbreak baseline) at 0.68, and human-curated AIR-Bench at 0.38 — i.e., synthetic-without-human-curation approached but did not match human-curated diversity, and clearly beat a single-technique automated baseline. However, independent NLG literature (arXiv:2504.12522) documents that embedding-cosine-similarity diversity metrics are gameable — a low-quality or incoherent generator can score as "more diverse" than a coherent one purely because incoherent outputs embed further apart, not because they're better test cases.

**Why it matters for Kelvran:** if Kelvran ever measures a candidate synthetic-generation pipeline's output diversity as a quality signal (e.g., "are these guardrail-bypass attempts meaningfully different from each other, not just paraphrases"), a bare cosine-similarity number is not sufficient evidence of quality on its own — it must be paired with a coherence/validity check (see Finding 3's anti-gaming filters), or a generator optimizing for "looks diverse" could quietly degrade case quality while improving the one metric being watched.

**Confidence:** Medium — the AutoRedTeamer number itself is a single peer-reviewed primary source (2-1 vote, verified against the actual PDF), and the gameability caveat is well-documented in secondary NLG literature but not tested against this specific paper's claim; no source disputes the numbers themselves.

**Sources:** AutoRedTeamer (NeurIPS 2025); arXiv:2504.12522 (diversity-metric gameability).

**build_now / not_yet: not_yet.** Trigger: only relevant once Kelvran has an actual generation pipeline whose output diversity needs measuring — this is a measurement-methodology caveat to apply *when* that happens, not a standalone thing to build.

---

### 3. Real production pipelines add specific anti-gaming filters beyond basic legitimacy checks — rejecting trivial-transform wrapping and non-plausible-conversation shapes — precisely because raw LLM generators can be gamed into degenerate cases (High confidence)

**What:** Beyond OpenAI GPT-Red's baseline legitimacy validation (Finding 1), the paper documents two additional, specific anti-reward-hacking filters added because the baseline wasn't sufficient: (1) reject attacks that merely wrap already-disallowed content in a superficial transformation (e.g., "translate this disallowed text") since these "do not provide materially new information," and (2) reject attacks that don't resemble plausible real user conversations. These were added *in response to* observed gaming of the reward signal by the self-play attacker, not designed upfront speculatively.

**Why it matters for Kelvran:** this is the single most directly actionable, concrete precedent for "how do you validate synthetic case quality before promoting it into a CI-blocking gate" — it names the exact failure modes a naive LLM-generation pipeline for Kelvran's guardrail category would likely produce (a generated "bypass attempt" that's just a known-bad string with a cosmetic wrapper, or a syntactically valid but conversationally implausible probe no real attacker or user would ever send) and shows both are real enough in practice to need dedicated filters, discovered iteratively rather than anticipated in full upfront.

**Confidence:** High — primary OpenAI technical paper, both filters unanimous 3-0, direct textual match to the quoted mechanism.

**Sources:** GPT-Red (OpenAI, 2026).

**build_now / not_yet: build_now**, but scoped narrowly: if/when Kelvran builds any LLM-assisted case-generation *prototype* (even a manual, human-reviewed-every-case first pass — see the Open Questions section), encode these two filters as an explicit design requirement from day one, before any generation code is written, rather than discovering the same gaming behavior the hard way later. This is a design-constraint precondition, not a green light to build the generation pipeline itself yet.

---

### 4. For prompt-injection specifically — Kelvran's own highest-priority OWASP LLM01 category — OpenAI reports its automated red-teamer beating human red-teamers on a held-out 2025 benchmark; this is the strongest single data point that automated generation can outperform hand-authored cases on this exact category (High confidence)

**What:** On the IPI Challenge 2025 dataset (a held-out indirect-prompt-injection benchmark originally containing human-written attacks), OpenAI removed the original human-written attack for each case and generated a replacement using GPT-Red, then measured average attack success rate (ASR) against a fixed defender model. GPT-Red achieved the highest average ASR of any approach tested, "substantially outperforming human red-teamers" (roughly 80-85% ASR vs. ~13% for human red-teamers in the paper's own figure). The paper's own footnote caveats that this does not imply universal superiority — humans may find novel scenarios GPT-Red's training distribution doesn't cover — but the specific, measured result stands.

**Why it matters for Kelvran:** this is the one finding in this entire research pass that speaks directly to the research question's central ask — is there a real, credible signal that automated generation can find more/better prompt-injection bypasses than hand-authored ones, for the exact threat class (OWASP LLM01) Kelvran names as top priority. The answer, per this specific (single-source, single-lab) data point, is yes on this exact category, with an explicit acknowledgment that it doesn't generalize to "humans are obsolete for this" — a scope-limited yes, not an unqualified one.

**Confidence:** High on what the paper reports (direct primary-source PDF read, exact figures matched); the finding's generalizability beyond OpenAI's own held-out benchmark and defender model is unverified by any independent replication.

**Sources:** GPT-Red (OpenAI, cdn.openai.com/pdf/gpt-red-automated-red-teaming-via-self-play-at-scale.pdf).

**build_now / not_yet: not_yet.** Trigger: this is evidence to weigh, not a system Kelvran can adopt directly — OpenAI has not published GPT-Red as a tool, and there is no drop-in equivalent. The actionable takeaway is "prompt-injection is provably a category where LLM-generated attacks can be materially more effective than a small hand-authored set" — which raises the priority of eventually growing this category, but the *how* still requires the validation infrastructure named in Finding 3 to exist first, and Kelvran's own guardrail corpus (24 cases) is far smaller than the benchmark scale in this paper, so no direct extrapolation of the ASR delta to Kelvran's specific gateway/guardrail implementation should be assumed without dedicated testing.

---

### 5. Multiple published automated red-teaming papers explicitly do NOT treat prompt injection as a standalone category — it's usually a trigger mechanism for other harms, not a first-class target (Medium confidence)

**What:** A six-category automated red-teaming paper (testing GPT-OSS-20B) explicitly states that "prompt injection and role-play escalation are treated as mechanisms that may trigger these categories [reward hacking, deceptive alignment, data exfiltration, sandbagging, inappropriate tool use, chain-of-thought manipulation], not as separate experimental categories" — prompt injection appears only as a means to another end within that paper's taxonomy, never optimized on its own.

**Why it matters for Kelvran:** this is a genuine tension with Finding 4 that the research should not paper over. OpenAI's GPT-Red treats prompt injection as a first-class, directly-optimized, benchmarked category (with its own held-out dataset) and shows strong results there specifically. This other paper's design choice — subsuming prompt injection into six *other* categories as a trigger mechanism — suggests that at least some of the current academic automated-red-teaming literature does not treat prompt-injection-as-such as a category worth separately optimizing, which could mean either (a) it's considered "solved enough" not to need separate treatment in general threat-model research, or (b) building a *good* dedicated prompt-injection generator is harder or less standardized than treating it as a byproduct of other attacks. The research does not resolve which explanation is correct, and Kelvran should not assume either without further digging.

**Confidence:** Medium — a single paper's explicit design choice, unanimous 3-0 vote on the textual claim itself, but the *interpretation* of why the paper made this choice is not independently corroborated.

**Sources:** arxiv.org/html/2512.20677v5 (six-category automated red-teaming paper).

**build_now / not_yet: not_yet.** Trigger: none identified — this is a caveat to carry into any future generator-design decision (don't assume a generic multi-category automated red-teamer will naturally produce good standalone prompt-injection cases; it may need to be explicitly, separately optimized for that category the way GPT-Red was), not an action item on its own.

---

### 6. Promptfoo ships a real, productionized prompt-injection/guardrail-bypass plugin taxonomy directly relevant to Kelvran's OWASP LLM01 gap, generated via a cloud-assisted LLM pipeline with iterative refinement (High confidence)

**What:** Promptfoo's red-team plugins include `indirect-prompt-injection` (injected instructions within prompt variables), `ascii-smuggling` (obfuscating malicious content via ASCII tricks), `system-prompt-override` (manipulating a model to ignore/override its system prompt), `cca`/Context Compliance Attack (manipulated chat history), and `special-token-injection` (conversation-format delimiter exploitation) — a real, currently-shipping taxonomy for exactly Kelvran's flagged threat class. Generation is a two-phase, cloud-assisted flow: an initial batch of adversarial payloads is generated by AI models (defaulting to a remote cloud service, not local, for quality reasons — promptfoo's own docs disclose that local-only generation "may result in lower quality adversarial inputs"), then, if no vulnerability is found, the client generates follow-up attacks using context from prior failed attempts, escalating sophistication until a vulnerability is found or a max-attempt limit is reached.

**Why it matters for Kelvran:** this demonstrates a real, currently-maintained, commercially-supported taxonomy and generation loop for the exact category Kelvran cares about, and is the closest thing to an off-the-shelf tool Kelvran could evaluate for its guardrail category specifically — the iterative-refinement pattern (regenerate using prior-attempt context, escalate until success or cap) is directly analogous to the "grow the corpus with what actually defeats the target" pattern Kelvran would want if it built its own generator.

**Confidence:** High — primary official product documentation, directly and repeatedly verified live, unanimous/near-unanimous votes across two related claims.

**Sources:** promptfoo.dev/docs/red-team/architecture/, promptfoo.dev/docs/red-team/plugins/.

**build_now / not_yet: build_now** — as an *evaluation*, not an adoption. Trigger: before building any custom generator for the guardrail category, spend a bounded evaluation cycle running promptfoo's existing prompt-injection/guardrail-bypass plugins against Kelvran's actual gateway/guardrail implementation in a sandboxed, non-CI-gated environment, specifically to see whether it surfaces bypass patterns not already covered by the 24 hand-authored guardrail cases. This is cheap (an existing tool, not new infrastructure) and directly tests whether "off-the-shelf red-teaming finds gaps in our specific guardrail" before any bespoke build is justified.

---

### 7. Promptfoo's own documented pipeline has NO pre-use quality gate on generated test probes — the closest thing it has to "validate before promoting to a regression suite" is a bare empirical pass/fail on the target, not a dedup/quality/mislabeling audit (High confidence)

**What:** Promptfoo's "Evaluation Engine" validates the *target's response* to a generated attack (via vulnerability detectors and LLM-as-a-judge grading, defaulting to `gpt-5`) — it does not validate the generated attack probe itself before using it as a test. There is no documented feature for deduplicating, quality-checking, or reviewing generated probes before execution; the CLI workflow bundles generation and execution with no pause/approval gate between them. For promoting a successful attack into a persistent regression suite, promptfoo's "Retry" strategy is empirical-success-gated only — "automatically incorporates previously failed test cases into your test suite" (meaning cases where the target failed to resist the attack), with no separate LLM-judge quality/dedup audit of the case itself before it becomes a permanent regression fixture.

**Why it matters for Kelvran:** this is the central risk the research question asks about, confirmed directly against the most mature production red-team tool researched. If Kelvran adopted promptfoo's generation+promotion model as-is, the only gate between "an LLM generated this adversarial probe" and "this probe is now a permanent regression case that can fail real PRs" is whether it happened to defeat the target once — no check that the probe is non-garbage, non-duplicate, or correctly labeled (i.e., that a "guardrail should have blocked this" case actually has a guardrail-should-block-it ground truth, the exact defect class `audit_corpus.py` already exists to catch on Kelvran's *hand-authored* cases). Promoting synthetic cases through this pattern without adding Kelvran's own validation layer would import synthetic-data-quality risk directly into the CI-blocking Wilson-lower-bound gate.

**Confidence:** High — four independent primary-source docs pages fetched and cross-checked, unanimous on the absence of a pre-use quality gate.

**Sources:** promptfoo.dev/docs/red-team/architecture/, promptfoo.dev/docs/red-team/ (overview), promptfoo.dev/docs/red-team/plugins/intent/, promptfoo.dev/docs/red-team/strategies/, promptfoo.dev/docs/red-team/troubleshooting/grading-results/.

**build_now / not_yet: not_yet — and this is the report's central negative finding.** Trigger: Kelvran must NOT wire any synthetic-case generator (whether promptfoo, a custom self-play system, or anything else) directly into `regression_corpus_guardrail.json` or any other `regression_corpus_*.json` file consumed by the `--fail-under` gate until a dedicated validation step exists that is stricter than "it defeated the target once." The precondition for building that gate: extend `audit_corpus.py`'s existing design-defect-audit pattern (ambiguous task framing, wrong/unverifiable ground truth) to run specifically on *candidate* synthetic cases before promotion, and keep it report-only/human-gated for at least one full review cycle — mirroring the same "instrument first, act once there's real data" precedent `audit_corpus.py` itself already cites from `docs/upgrade-research/evals-next-upgrade-round2-2026-09-09.md`.

---

### 8. Academic self-play pipelines can be even less rigorous than promptfoo about per-sample validation — a real precedent for how synthetic-data quality risk compounds if validation is skipped (High confidence)

**What:** Safety Self-Play's (ACL Findings 2026) reward computation and Experience Pool storage/eviction are gated *solely* by a single automated LLM-judge's discrete 1-5 safety score — no deduplication, similarity check, or human review is applied to generated attack/defense samples before they're used for reward or stored for replay. The paper explicitly states it does not use diversity/dedup rewards, relying entirely on self-play dynamics for variety. The only human-review language in the entire paper is a citation justifying that LLM-judge protocols *in general* have been validated by prior work via human inspection — not an active human check on this pipeline's own output.

**Why it matters for Kelvran:** this is a cautionary precedent, not a template to follow. It shows that even a peer-reviewed 2026 self-play system can ship with a single automated judge as the *sole* arbiter of sample quality and no dedup mechanism at all — and that this gap is easy to miss because the paper's abstract and headline results don't advertise it. If Kelvran ever adopts a self-play-style approach for guardrail-bypass generation, this is a concrete example of the exact under-validation the research question worries about, occurring in a legitimate, currently-cited academic system — a strong argument for Kelvran building its own validation layer deliberately rather than copying a self-play architecture wholesale and assuming its internal judge is sufficient.

**Confidence:** High — direct read of the primary paper's algorithm (not just its abstract), unanimous 3-0.

**Sources:** Safety Self-Play (ACL Findings 2026).

**build_now / not_yet: not_yet.** Trigger: none — this is a design anti-pattern to avoid, reinforcing Finding 7's precondition (do not let a single LLM judge alone gate promotion into the regression corpus; require dedup/mislabeling checks as a separate step, since "an LLM judge already validated it" is not equivalent to "it's not a duplicate or garbage case").

---

### 9. Manual/crowdsourced red-teaming at scale is a real, distinct alternative already proven in this space — not obsolete, and a lower-risk lever than automated generation if Kelvran just wants more cases faster (Medium confidence)

**What:** Anthropic released a public dataset of 38,961 human red-team attacks (Ganguli et al., 2022), collected from crowdworkers on Upwork and Amazon Mechanical Turk — a genuinely large-scale, human-generated, non-LLM-self-play corpus, demonstrating that "more red-team cases, faster" does not require automated generation at all.

**Why it matters for Kelvran:** this is a useful contrast point the research question explicitly asked for. Kelvran's regression corpus is entirely hand-authored today (137 cases) — growing it via more structured *human* red-teaming (e.g., a scoped bug-bounty-style internal exercise specifically targeting the guardrail category, mirroring how `evals/evals/audit_corpus.py`'s design-defect categories were presumably first identified) carries none of the synthetic-data-trust risk this whole report is about, at the cost of being slower and not benefiting from GPT-Red-style scale/ASR advantages (Finding 4). It is a real, lower-risk lever available today, independent of any synthetic-generation decision.

**Confidence:** Medium-High — the historical fact itself is well-sourced (primary paper + dataset card), but its relevance as a *recommendation* for Kelvran is this report's own inference, not a sourced claim.

**Sources:** arxiv.org/abs/2209.07858 (Anthropic, Ganguli et al. 2022).

**build_now / not_yet: build_now.** Trigger: none required — this needs no new validation infrastructure since it's the same hand-authoring process the corpus already uses today. If Kelvran wants to grow the guardrail category faster without touching the synthetic-data-trust question at all, running a scoped internal red-team exercise (or using promptfoo's plugins per Finding 6 purely as an *idea generator* for a human to then hand-write/verify each case, never auto-promoting) is available immediately with zero new risk to the corpus's trustworthiness.

---

## Direct answer to the research question

**Is there a real, safe way to auto-generate new adversarial guardrail-bypass attempts to grow the guardrail/prompt-injection category?** Yes, the generation techniques are real (Findings 1, 4, 6) and prompt injection specifically is a category where automated generation has beaten human red-teamers in at least one credible, primary-sourced benchmark (Finding 4). But "safe" in the sense the question means it — safe to promote into a CI-blocking gate — requires validation infrastructure Kelvran does not have today, and every credible precedent surveyed (AutoRedTeamer's ASR-threshold gate, GPT-Red's mixed programmatic+LLM legitimacy checks plus two specific anti-gaming filters) builds that gate as a first-class, non-optional part of the system, not an afterthought. The one commercial tool with a *shipped* prompt-injection taxonomy (promptfoo) has no such gate for promoting generated cases into a persistent suite — it only checks whether the attack worked once. **Net: the generation techniques are proven; the validation techniques required to make them safe for Kelvran's specific CI-blocking use case are also proven (elsewhere) but not yet built here, and building generation before validation would be building the risky half first.**

## What would genuinely degrade the regression corpus's trustworthiness

1. **Wiring any generator (custom or off-the-shelf) directly into a `regression_corpus_*.json` file without a validation step stricter than "it defeated the target once."** This is Finding 7's exact failure mode, demonstrated as promptfoo's actual current behavior.
2. **Trusting a single LLM judge as the sole gate**, the way Safety Self-Play does (Finding 8) — `audit_corpus.py`'s own design already avoids exactly this trap for hand-authored cases by staying deliberately separate from the judging mechanism; a synthetic-case validator must preserve that same separation, not collapse it for convenience.
3. **Treating a diversity/quality metric (e.g., embedding cosine similarity) as sufficient evidence of quality** (Finding 2) without a coherence/plausibility check alongside it — a generator could be tuned to score well on the metric being watched while actually degrading case quality.
4. **Assuming a generic multi-category automated red-teamer will produce good standalone prompt-injection cases** (Finding 5) without dedicated optimization for that category specifically, given at least one credible paper explicitly declined to treat prompt injection as a first-class category at all.

## Caveats

- Finding 4 (GPT-Red beating humans on IPI-2025) is a single-lab, single-paper result on OpenAI's own held-out benchmark and defender model; it is strong evidence that automated prompt-injection generation *can* outperform hand-authored cases in principle, but it should not be read as "any automated generator will beat Kelvran's 24 hand-authored guardrail cases" — no equivalent test has been run against Kelvran's actual gateway/guardrail implementation.
- Several claims researched but refuted are worth naming explicitly since they represent plausible-sounding but unverified hypotheses this report does NOT rely on: automated generation was NOT confirmed to find substantially more real vulnerabilities than hand-authored cases *on identical categories* (0-3 vote — the opposite of what Finding 4 shows for one specific category, underscoring that Finding 4's result is category-specific, not general); promptfoo's plugins were NOT confirmed to be full LLM-as-red-teamer generators in the "specialized model per category" sense claimed; and OpenAI's own "mixed methods, seed-then-scale" recommendation and DALL-E 3 second-classifier precedent were both refuted at 0-3, meaning this report cannot cite OpenAI as having a documented, generalized "always validate synthetic prompts with a second classifier before production use" policy beyond GPT-Red's own specific pipeline.
- Web-search tooling (Exa, Tavily) hit rate limits repeatedly during verification; several findings rest on direct primary-source PDF/doc reads rather than broad independent secondary-source corroboration. This is a stronger evidentiary bar for *what the source says*, but a weaker bar for *whether other unfound sources disagree*.
- Time-sensitivity: promptfoo's documented behavior (Findings 6-7) reflects live docs as of 2026-09-13 and could change; the academic papers (AutoRedTeamer NeurIPS 2025, Safety Self-Play ACL Findings 2026, GPT-Red 2026) are all recent enough not to be stale, but none has had time to accumulate independent replication or critique in the literature yet.

## Open questions

1. Should Kelvran's first move be evaluating promptfoo's existing plugins against the live guardrail implementation (Finding 6, cheap, no new infra) before considering any bespoke generator — and if that surfaces real gaps, does closing them via more hand-authored cases (Finding 9) resolve the immediate need without ever building a synthetic-generation pipeline at all?
2. What would a validation layer analogous to `audit_corpus.py` — but designed for *candidate* cases, not existing ones — actually look like architecturally, and should it be a new module or an extension of `audit_corpus.py` itself? (Its current docstring explicitly scopes it to auditing existing corpus entries, not screening new candidates.)
3. Is there a credible middle path where an LLM proposes candidate guardrail-bypass cases but a human always reviews and hand-verifies each one before it enters `regression_corpus_guardrail.json` — i.e., LLM-assisted authoring rather than autonomous generation — and would that sidestep most of Finding 7/8's risk while still speeding up corpus growth?
4. Does Kelvran have (or need) a non-CI-gated "candidate corpus" staging area where synthetic or newly-proposed cases could accumulate and be observed for a review period before ever being eligible for promotion into the `--fail-under`-gated files — mirroring the "instrument first, act once there's real data" precedent already established for `audit_corpus.py`?
