"""One-off, out-of-band research script validating whether a candidate
embedding-similarity cache-hit gate would be worth building, WITHOUT
production traffic and WITHOUT touching any production code.

Per `docs/upgrade-research/cache-semantic-embedding-readiness-round4-2026-09-11.md`'s
Finding 1 (BUILD NOW) / Recommendation #1: real embedding-based semantic L3
caching remains correctly deferred pending production miss-telemetry (see
`STATUS.md`'s "Blocked, unchanged" list) — this script does NOT build that
feature. It answers a narrower, cheaper question: if Kelvran ever did build
one, would a bare cosine-similarity threshold be trustworthy, and how much
does layering the EXISTING entity/date fingerprint hard gate
(`gateway/internal/gateway/dataplane/entities.go`) on top actually recover?

Deliberately lives OUTSIDE the `evals` Python package (`pyproject.toml`'s
`[tool.hatch.build.targets.wheel]` only packages `evals/`, so this
directory is never installed, never subject to `.importlinter`'s layer
contract, and never ships as part of `kelvran-evals`) — it imports FROM
`evals` (already installed in this venv) but is not part of it, matching
the research doc's own "lives in evals/ ... never in gateway/'s Go
dependency graph, never a permanent CLI feature" framing.

## Evaluation set

12 pairs, built from 4 real, natural-language seed scenarios already
present in `evals/tests/fixtures/regression_corpus_cache_adversarial.json`
-- the corpus's own already-known "must never be treated as a safe cache
hit" pairs are reused directly as ground-truth NEGATIVES (never
re-labeled or softened), rather than fabricating synthetic negatives from
scratch for failure modes this codebase has already found and documented:

- `adversarial-cache-negation-polarity-flip-drug-administration` --
  EXACTLY the "withhold" -> "administer" meaning-reversal
  `docs/upgrade-research/cache-semantic-embedding-readiness-round4-2026-09-11.md`'s
  Finding 2 cites (cosine 0.9608 in that paper's own measurement) as the
  critical failure mode a real embedding gate must not approve.
- `adversarial-cache-referent-shift-named-entity` (Drug X -> Drug Y).
- `adversarial-cache-referent-shift-unnamed-entity` (antibiotic ->
  antiviral).
- `adversarial-cache-entity-gate-number-change-regression` (a changed
  numeric threshold).

For each of the 4 seeds, an LLM call (`evals.judge.providers`'s existing
Bedrock wiring, no new provider) generates ONE genuine paraphrase (a real
POSITIVE -- same meaning, different wording) and ONE separate "distinct
but related" negative (a real NEGATIVE, mirroring Redis LangCache's own
generator-pair prompt pattern) -- 4 seeds x (1 corpus-negative + 1
LLM-paraphrase + 1 LLM-negative) = 12 pairs total.

## What this produces that Kelvran currently has zero of

A real precision/recall table, at several candidate cosine thresholds,
both BARE and LAYERED with a faithful Python port of the Go entity/date
fingerprint gate (`entity_fingerprint` below) -- directly answering
Finding 1's own framed question: "would a real embedding layer even catch
meaningfully more paraphrases than the existing Jaccard/MinHash L3-lite
already does, at what precision cost."

Run for real (requires live AWS Bedrock credentials, e.g. `evals/.env`):

    uv run python scripts/validate_embedding_gate.py
"""

from __future__ import annotations

import asyncio
import json
import os
import re
from dataclasses import dataclass
from pathlib import Path

_NUMBER_PATTERN = re.compile(r"\$?\d+(?:\.\d+)?%?")
_DATE_PATTERN = re.compile(
    r"\d{4}-\d{2}-\d{2}"
    r"|\d{1,2}/\d{1,2}/\d{2,4}"
    r"|(?:January|February|March|April|May|June|July|August|September|October"
    r"|November|December|Jan|Feb|Mar|Apr|Jun|Jul|Aug|Sept?|Oct|Nov|Dec)\.?\s+"
    r"\d{1,2}(?:st|nd|rd|th)?(?:,?\s+\d{4})?"
)
_CAPITALIZED_SEQUENCE_PATTERN = re.compile(r"\b[A-Z][a-zA-Z]*(?:\s+[A-Z][a-zA-Z]*)*\b")
_ALWAYS_CAPITALIZED_WORDS = {"I"}
_FIRST_WHITESPACE_PATTERN = re.compile(r"[ \t\n]")

TITAN_EMBED_MODEL_ID = "amazon.titan-embed-text-v2:0"


def _capitalized_sequences(turn: str) -> list[str]:
    """Python port of `entities.go`'s `capitalizedSequences`: excludes a
    turn's own first word (sentence-initial capitalization is a grammar
    artifact, e.g. "What is...", not an entity signal) -- ported
    per-turn, matching the Go function's own per-message application.
    """
    match = _FIRST_WHITESPACE_PATTERN.search(turn)
    if match is None:
        return []  # the entire turn is one word -- already excluded as "first word"
    rest = turn[match.end() :]
    matches = _CAPITALIZED_SEQUENCE_PATTERN.findall(rest)
    return [m for m in matches if m not in _ALWAYS_CAPITALIZED_WORDS]


def entity_fingerprint(turns: list[str]) -> frozenset[str]:
    """Python port of `entities.go`'s `Fingerprint` -- Cache L3-lite's
    real hard gate, ported here ONLY for this offline research harness
    (never imported by, or shared with, gateway's own Go code; this is a
    faithful re-implementation for validation purposes, not the
    production gate itself). `turns` is one string per conversation
    turn, so `_capitalized_sequences`' own first-word exclusion applies
    per turn, exactly as the Go original applies it per `adapter.Message`.
    """
    fingerprint: set[str] = set()
    for turn in turns:
        fingerprint.update(_NUMBER_PATTERN.findall(turn))
        fingerprint.update(_DATE_PATTERN.findall(turn))
        fingerprint.update(_capitalized_sequences(turn))
    return frozenset(fingerprint)


def cosine_similarity(a: list[float], b: list[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b, strict=True))
    norm_a = sum(x * x for x in a) ** 0.5
    norm_b = sum(y * y for y in b) ** 0.5
    if norm_a == 0 or norm_b == 0:
        return 0.0
    return dot / (norm_a * norm_b)


@dataclass(frozen=True)
class EvalPair:
    """One (text_a, text_b) pair with a ground-truth label: should_match
    is True only for a genuine, safe-to-serve-from-cache paraphrase --
    never for the corpus's own already-known adversarial negatives, no
    matter how similar the two texts read on the surface.
    """

    scenario: str
    kind: str  # "corpus_negative" | "llm_paraphrase" | "llm_negative"
    text_a: list[str]  # conversation turns
    text_b: list[str]
    should_match: bool


_PARAPHRASE_NEGATIVE_PROMPT = """\
You are helping build a research evaluation set for testing an \
LLM-response cache's similarity gate.

Given the ORIGINAL query below, produce TWO outputs:

1. A PARAPHRASE: a genuine, semantically-equivalent rewording of the \
ORIGINAL -- same question, same real-world meaning, different wording/\
phrasing/sentence structure. A response cached for the ORIGINAL would \
be a CORRECT answer to the PARAPHRASE too.
2. A NEGATIVE: a query that is topically related and superficially \
similar in subject matter to the ORIGINAL, but asks something \
MEANINGFULLY DIFFERENT -- a response cached for the ORIGINAL would be \
WRONG or MISLEADING if served for this NEGATIVE.

ORIGINAL: {original}

Respond in exactly this format, each on its own line, no other text:
PARAPHRASE: <your paraphrase>
NEGATIVE: <your negative>
"""


def build_paraphrase_negative_prompt(original: str) -> str:
    return _PARAPHRASE_NEGATIVE_PROMPT.format(original=original)


_PARAPHRASE_PATTERN = re.compile(
    r"PARAPHRASE:\s*(.*?)\s*(?:\n\s*NEGATIVE:|\Z)", re.DOTALL
)
_NEGATIVE_PATTERN = re.compile(r"NEGATIVE:\s*(.*)", re.DOTALL)


@dataclass(frozen=True)
class ParaphraseNegative:
    paraphrase: str
    negative: str


def parse_paraphrase_negative_response(raw: str) -> ParaphraseNegative:
    paraphrase_match = _PARAPHRASE_PATTERN.search(raw)
    negative_match = _NEGATIVE_PATTERN.search(raw)
    if paraphrase_match is None or negative_match is None:
        raise ValueError(
            f"response missing PARAPHRASE:/NEGATIVE: labeled lines: {raw!r}"
        )
    return ParaphraseNegative(
        paraphrase=paraphrase_match.group(1).strip(),
        negative=negative_match.group(1).strip(),
    )


@dataclass(frozen=True)
class PrecisionRecall:
    threshold: float
    use_entity_gate: bool
    true_positives: int
    false_positives: int
    true_negatives: int
    false_negatives: int

    @property
    def precision(self) -> float | None:
        denom = self.true_positives + self.false_positives
        return self.true_positives / denom if denom else None

    @property
    def recall(self) -> float | None:
        denom = self.true_positives + self.false_negatives
        return self.true_positives / denom if denom else None


def score_at_threshold(
    pairs: list[EvalPair],
    embeddings: dict[str, list[float]],
    threshold: float,
    use_entity_gate: bool,
) -> PrecisionRecall:
    tp = fp = tn = fn = 0
    for pair in pairs:
        joined_a = " ".join(pair.text_a)
        joined_b = " ".join(pair.text_b)
        similarity = cosine_similarity(embeddings[joined_a], embeddings[joined_b])
        predicted_match = similarity >= threshold
        if use_entity_gate and predicted_match:
            predicted_match = entity_fingerprint(pair.text_a) == entity_fingerprint(
                pair.text_b
            )

        if predicted_match and pair.should_match:
            tp += 1
        elif predicted_match and not pair.should_match:
            fp += 1
        elif not predicted_match and not pair.should_match:
            tn += 1
        else:
            fn += 1
    return PrecisionRecall(
        threshold=threshold,
        use_entity_gate=use_entity_gate,
        true_positives=tp,
        false_positives=fp,
        true_negatives=tn,
        false_negatives=fn,
    )


# The 4 real, natural-language adversarial scenarios in the existing
# corpus this script's evaluation set is seeded from -- see this module's
# own docstring for why each was chosen (real, already-documented "must
# not match" failure modes, never fabricated).
_SEED_CASE_IDS = (
    "adversarial-cache-negation-polarity-flip-drug-administration",
    "adversarial-cache-referent-shift-named-entity",
    "adversarial-cache-referent-shift-unnamed-entity",
    "adversarial-cache-entity-gate-number-change-regression",
)

_FIXTURE_PATH = (
    Path(__file__).parent.parent
    / "tests"
    / "fixtures"
    / "regression_corpus_cache_adversarial.json"
)


def load_seed_scenarios(
    fixture_path: Path = _FIXTURE_PATH,
) -> list[tuple[str, list[str], list[str]]]:
    """Return `(scenario_id, cached_query_turns, new_query_turns)` for
    each of `_SEED_CASE_IDS`, read directly from the real, live corpus
    fixture -- never a frozen copy of its text -- so this script tracks
    the corpus if it's ever edited, rather than silently drifting from it.
    """
    cases = json.loads(fixture_path.read_text())
    by_id = {c["id"]: c for c in cases}
    scenarios = []
    for case_id in _SEED_CASE_IDS:
        case = by_id[case_id]
        cached_turns = [
            m["content"]
            for m in case["task_spec"]["cached_query"]["messages"]
            if m["role"] == "user"
        ]
        new_turns = [
            m["content"]
            for m in case["task_spec"]["new_query"]["messages"]
            if m["role"] == "user"
        ]
        scenarios.append((case_id, cached_turns, new_turns))
    return scenarios


async def build_evaluation_set(call_model) -> list[EvalPair]:
    pairs: list[EvalPair] = []
    for scenario_id, cached_turns, new_turns in load_seed_scenarios():
        pairs.append(
            EvalPair(
                scenario=scenario_id,
                kind="corpus_negative",
                text_a=cached_turns,
                text_b=new_turns,
                should_match=False,
            )
        )
        # Generate against the seed's LAST turn only -- the referent-
        # shift scenarios' own second turn ("is it safe for adults?")
        # has no content of its own to paraphrase; the entity/topic lives
        # in the first turn.
        seed_text = cached_turns[0]
        raw = await call_model(build_paraphrase_negative_prompt(seed_text))
        parsed = parse_paraphrase_negative_response(raw)

        paraphrase_turns = [parsed.paraphrase, *cached_turns[1:]]
        pairs.append(
            EvalPair(
                scenario=scenario_id,
                kind="llm_paraphrase",
                text_a=cached_turns,
                text_b=paraphrase_turns,
                should_match=True,
            )
        )
        negative_turns = [parsed.negative, *cached_turns[1:]]
        pairs.append(
            EvalPair(
                scenario=scenario_id,
                kind="llm_negative",
                text_a=cached_turns,
                text_b=negative_turns,
                should_match=False,
            )
        )
    return pairs


def _resolve_bedrock_client():
    import boto3
    from botocore.config import Config as BotoConfig

    resolved_region = os.environ.get("AWS_REGION") or os.environ.get(
        "AWS_DEFAULT_REGION"
    )
    return boto3.client(
        "bedrock-runtime",
        region_name=resolved_region,
        config=BotoConfig(read_timeout=300),
    )


def embed_text_sync(text: str, client) -> list[float]:
    body = json.dumps({"inputText": text, "dimensions": 1024, "normalize": True})
    response = client.invoke_model(modelId=TITAN_EMBED_MODEL_ID, body=body)
    payload = json.loads(response["body"].read())
    return payload["embedding"]


async def embed_all(texts: set[str], client) -> dict[str, list[float]]:
    embeddings: dict[str, list[float]] = {}
    for text in texts:
        embeddings[text] = await asyncio.to_thread(embed_text_sync, text, client)
    return embeddings


async def main() -> None:
    from evals.judge.providers import BEDROCK_SONNET_5_MODEL_ID, make_bedrock_call_model

    call_model = make_bedrock_call_model(BEDROCK_SONNET_5_MODEL_ID)
    pairs = await build_evaluation_set(call_model)

    client = _resolve_bedrock_client()
    all_texts = {" ".join(pair.text_a) for pair in pairs} | {
        " ".join(pair.text_b) for pair in pairs
    }
    embeddings = await embed_all(all_texts, client)

    n_seeds = len(_SEED_CASE_IDS)
    print(f"Evaluation set: {len(pairs)} pairs from {n_seeds} seed scenarios\n")
    for threshold in (0.75, 0.80, 0.85, 0.90, 0.95):
        for use_entity_gate in (False, True):
            result = score_at_threshold(pairs, embeddings, threshold, use_entity_gate)
            gate_label = "bare" if not use_entity_gate else "+ entity gate"
            print(
                f"threshold={threshold:.2f} ({gate_label:14s}) "
                f"precision={result.precision} recall={result.recall} "
                f"tp={result.true_positives} fp={result.false_positives} "
                f"tn={result.true_negatives} fn={result.false_negatives}"
            )


if __name__ == "__main__":
    asyncio.run(main())
