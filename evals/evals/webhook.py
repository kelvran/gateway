"""HMAC-signed, retrying webhook delivery.

Per docs/upgrade-research/operator-alerting-integrations-2026-09-15.md
Finding 5: current industry consensus (Stripe, Svix, and the vendor-
neutral Standard Webhooks specification) converges on a small set of
baseline properties for *sending* a webhook responsibly -- HMAC-SHA256
signing, a stable per-event ID for receiver-side dedup, and a bounded
retry schedule ending in a give-up state rather than an unbounded loop.
``cli.py``'s ``trend_alert_cmd`` -- this module's one real caller -- had
none of these. This module applies them, replacing that command's own
single-attempt, unsigned ``urllib.request.urlopen`` call.

Deliberately kept synchronous, unlike gateway's own Go-side
``internal/alerting.WebhookNotifier`` (which sends from a background
goroutine so a slow receiver never blocks the request path): this is a
one-shot CLI command whose own process exits immediately after this
call returns, so there is no in-flight caller a background send would
protect -- the retry loop's own bounded wait IS this command's real
work, not overhead to hide from anything.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import json
import secrets
import time
import urllib.error
import urllib.request

DEFAULT_MAX_ATTEMPTS = 3
DEFAULT_BASE_BACKOFF_SECONDS = 0.5


def _sign_payload(secret: str, event_id: str, timestamp: str, body: bytes) -> str:
    """HMAC-SHA256 signature over ``id.timestamp.body``, per the Standard
    Webhooks specification (standardwebhooks.com/github.com/standard-
    webhooks/standard-webhooks), verified directly against that spec's
    own current text before implementing this, not assumed -- the
    identical scheme gateway/internal/alerting.signPayload implements
    in Go, so both deployables' webhook sends are mutually consistent
    for any receiver verifying either one. Returned as ``v1,<base64>``,
    that spec's own documented header value shape.

    A ``secret`` prefixed ``whsec_`` (the Standard Webhooks convention
    for a base64-encoded secret) is decoded before use as the HMAC key,
    for direct interoperability with a Standard-Webhooks-compliant
    receiver -- mirroring the Go sender's own identical convention. A
    secret in neither form is still accepted, used as raw UTF-8 bytes
    directly, for the same reason the Go sender accepts one: an
    operator sharing an arbitrary secret with their own receiver has no
    need to follow that exact convention.

    Raises ``ValueError`` if the secret is ``whsec_``-prefixed but the
    remainder isn't valid base64 -- an audit found the prior version
    silently fell back to signing with the raw, still-``whsec_``-
    prefixed string as the key on a decode failure, producing a
    signature that would fail receiver-side verification with zero
    diagnostic. ``validate=True`` closes the other half of that same
    gap: b64decode's own default silently strips non-alphabet
    characters instead of raising for most malformed input, which
    would have made this decode failure hard to actually trigger even
    with the fallback removed.
    """
    key = secret.encode("utf-8")
    if secret.startswith("whsec_"):
        try:
            key = base64.b64decode(secret[len("whsec_") :], validate=True)
        except (ValueError, binascii.Error) as err:
            msg = f"webhook secret is whsec_-prefixed but not valid base64: {err}"
            raise ValueError(msg) from err
    to_sign = f"{event_id}.{timestamp}.".encode() + body
    mac = hmac.new(key, to_sign, hashlib.sha256).digest()
    return "v1," + base64.b64encode(mac).decode("ascii")


def send_webhook(
    url: str,
    event_type: str,
    data: dict[str, object],
    *,
    secret: str | None = None,
    max_attempts: int = DEFAULT_MAX_ATTEMPTS,
    timeout: float = 10.0,
) -> None:
    """POST ``data`` to ``url``, wrapped in a Standard-Webhooks-shaped
    envelope (``{"id", "type", "timestamp", "data"}``, matching the
    exact same envelope shape gateway/internal/alerting.WebhookNotifier
    emits, for consistency across both deployables). Retries with
    bounded exponential backoff (base ``DEFAULT_BASE_BACKOFF_SECONDS``,
    doubling, up to ``max_attempts`` total attempts) plus up to 50%
    jitter, reusing the SAME event ID across every attempt so a
    receiver can recognize retries of one logical event, never a fresh
    one per attempt.

    Raises the LAST attempt's own exception if every attempt fails --
    never silently swallowed, matching this module's one real caller's
    own pre-existing "let a failed delivery raise as an unhandled
    exception" design intent, just after real retries now instead of a
    single attempt.

    A 4xx response is raised immediately, on the FIRST attempt, never
    retried -- an audit found the prior version retried a 4xx (bad
    signature, wrong URL, auth failure -- ``urllib.error.HTTPError`` is
    a subclass of ``urllib.error.URLError``, so it was previously
    caught by the same broad handler as a transient failure) identically
    to a 5xx or a network error, burning the full backoff schedule on a
    request guaranteed to fail identically every time. A 5xx and any
    other transport-level failure are still retried exactly as before.
    """
    event_id = "evt_" + secrets.token_hex(16)
    timestamp = str(int(time.time()))
    body = json.dumps(
        {
            "id": event_id,
            "type": event_type,
            "timestamp": timestamp,
            "data": data,
        }
    ).encode("utf-8")

    headers = {
        "Content-Type": "application/json",
        "webhook-id": event_id,
        "webhook-timestamp": timestamp,
    }
    if secret:
        headers["webhook-signature"] = _sign_payload(secret, event_id, timestamp, body)

    last_error: Exception | None = None
    rng = secrets.SystemRandom()
    for attempt in range(max_attempts):
        if attempt > 0:
            backoff = DEFAULT_BASE_BACKOFF_SECONDS * (2 ** (attempt - 1))
            time.sleep(backoff + rng.uniform(0, backoff / 2))
        request = urllib.request.Request(  # noqa: S310 -- url is operator-supplied (a CLI flag), never external/untrusted input
            url,
            data=body,
            headers=headers,
            method="POST",
        )
        try:
            urllib.request.urlopen(request, timeout=timeout)  # noqa: S310 -- same operator-supplied url
            return
        except urllib.error.HTTPError as err:
            if 400 <= err.code < 500:
                raise
            last_error = err
        except (urllib.error.URLError, OSError) as err:
            last_error = err

    assert last_error is not None  # noqa: S101 -- max_attempts >= 1 is enforced by the caller; this branch is only reached after >=1 failed attempt
    raise last_error
