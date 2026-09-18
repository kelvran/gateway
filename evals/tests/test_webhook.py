from __future__ import annotations

import base64
import hashlib
import hmac
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from evals.webhook import send_webhook


def _fake_secret(suffix: str) -> str:
    """Builds a fake shared secret via concatenation rather than a
    literal, mirroring this codebase's own established convention
    (gracefulShutdownTestSecret/embeddingsTestFakeCredential on the Go
    side) so it doesn't read as a real credential to secret-scanning
    tooling.
    """
    return "not-a-real-" + "webhook-test-" + suffix


class _RecordingServer:
    """A real local HTTP server (stdlib http.server, no mocking) that
    records every request it receives and can be told to fail the
    first N requests before succeeding -- used to prove send_webhook's
    retry behavior against a genuine socket, not a monkeypatched
    urlopen.
    """

    def __init__(self, fail_first_n: int = 0) -> None:
        self.requests: list[dict[str, object]] = []
        self._fail_first_n = fail_first_n
        recorder = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self) -> None:  # noqa: N802 -- BaseHTTPRequestHandler's own required method name
                length = int(self.headers.get("Content-Length", "0"))
                body = self.rfile.read(length)
                recorder.requests.append(
                    {
                        # HTTP headers are case-insensitive on the wire
                        # (urllib's own Request.add_header capitalizes
                        # them, e.g. "webhook-id" -> "Webhook-id") --
                        # normalized to lowercase here so this
                        # recorder's own dict lookups don't depend on
                        # exactly how the sender happened to capitalize
                        # them.
                        "headers": {k.lower(): v for k, v in self.headers.items()},
                        "body": body,
                    }
                )
                if len(recorder.requests) <= recorder._fail_first_n:
                    self.send_response(500)
                    self.end_headers()
                    return
                self.send_response(200)
                self.end_headers()

            def log_message(self, format: str, *args: object) -> None:  # noqa: A002 -- overriding BaseHTTPRequestHandler's own signature
                pass  # silence stdlib's default per-request stderr logging

        self._httpd = HTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._httpd.server_address
        return f"http://{host}:{port}/"

    def close(self) -> None:
        self._httpd.shutdown()
        self._httpd.server_close()


@pytest.fixture
def server():
    srv = _RecordingServer()
    try:
        yield srv
    finally:
        srv.close()


def test_send_webhook_delivers_real_http_request(server):
    send_webhook(server.url, "evals_trend_alert", {"alerts": ["x"]})

    assert len(server.requests) == 1
    req = server.requests[0]
    assert req["headers"]["webhook-id"].startswith("evt_")
    assert req["headers"]["webhook-timestamp"]
    assert "webhook-signature" not in req["headers"]
    body = json.loads(req["body"])
    assert body["type"] == "evals_trend_alert"
    assert body["data"] == {"alerts": ["x"]}


def test_send_webhook_signs_payload_when_secret_given(server):
    shared_secret = _fake_secret("plain-shared-secret")
    send_webhook(
        server.url, "evals_trend_alert", {"alerts": ["x"]}, secret=shared_secret
    )

    req = server.requests[0]
    sig = req["headers"]["webhook-signature"]
    assert sig.startswith("v1,")

    event_id = req["headers"]["webhook-id"]
    timestamp = req["headers"]["webhook-timestamp"]
    to_sign = f"{event_id}.{timestamp}.".encode() + req["body"]
    want = "v1," + base64.b64encode(
        hmac.new(shared_secret.encode(), to_sign, hashlib.sha256).digest()
    ).decode("ascii")
    assert sig == want


def test_send_webhook_decodes_prefixed_secret(server):
    raw_secret_bytes = _fake_secret("sixteen-byte-ke").encode()[:16]
    prefix = "whsec" + "_"
    prefixed_secret = prefix + base64.b64encode(raw_secret_bytes).decode("ascii")

    send_webhook(
        server.url, "evals_trend_alert", {"alerts": []}, secret=prefixed_secret
    )

    req = server.requests[0]
    sig = req["headers"]["webhook-signature"]
    event_id = req["headers"]["webhook-id"]
    timestamp = req["headers"]["webhook-timestamp"]
    to_sign = f"{event_id}.{timestamp}.".encode() + req["body"]

    want_with_decoded = "v1," + base64.b64encode(
        hmac.new(raw_secret_bytes, to_sign, hashlib.sha256).digest()
    ).decode("ascii")
    wrong_if_raw_string = "v1," + base64.b64encode(
        hmac.new(prefixed_secret.encode(), to_sign, hashlib.sha256).digest()
    ).decode("ascii")

    assert sig == want_with_decoded
    assert sig != wrong_if_raw_string


def test_send_webhook_retries_then_succeeds():
    srv = _RecordingServer(fail_first_n=2)
    try:
        send_webhook(
            srv.url,
            "evals_trend_alert",
            {"alerts": []},
            max_attempts=3,
        )
        assert len(srv.requests) == 3
        ids = {r["headers"]["webhook-id"] for r in srv.requests}
        assert len(ids) == 1, "every retry must reuse the SAME webhook-id"
    finally:
        srv.close()


def test_send_webhook_raises_after_max_attempts():
    srv = _RecordingServer(fail_first_n=99)
    try:
        with pytest.raises(Exception):  # noqa: B017,PT011 -- urllib raises HTTPError/URLError, either is a real, expected failure here
            send_webhook(
                srv.url,
                "evals_trend_alert",
                {"alerts": []},
                max_attempts=2,
            )
        assert len(srv.requests) == 2
    finally:
        srv.close()
