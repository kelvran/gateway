"""Lists/reads gatewayevents_v1 objects from object storage.

Per docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md: S3 today
(GCS is a structurally symmetric follow-on, not built here -- see that
RFC's Alternatives Considered). This module's job stops at raw bytes/
lines -- it never decodes a GatewayDecisionEvent itself. That stays
evals.ingestion.decode's exact job, called separately by whoever consumes
this module (see evals.cli's `ingest` command), per this project's own
`api/gatewayevents` "reuse, don't duplicate" contract discipline.
"""

from __future__ import annotations

import gzip
from collections.abc import Iterator
from urllib.parse import urlsplit

import boto3

_SUPPORTED_SCHEME = "s3"


def parse_object_storage_uri(source: str) -> tuple[str, str]:
    """Split an `s3://bucket/prefix` source into `(bucket, prefix)`.

    `prefix` is `""` when `source` names a bucket with no path at all
    (e.g. `s3://bucket`) -- a real, valid "list everything in the bucket"
    request, not an error. Any scheme other than `s3://` raises a clear
    `ValueError` rather than silently misbehaving -- see the RFC's own
    "GCS instead of S3" alternative for what a real `gs://` case would
    need to look like here.
    """
    parsed = urlsplit(source)
    if parsed.scheme != _SUPPORTED_SCHEME:
        raise ValueError(
            f"unsupported object-storage scheme {parsed.scheme!r} in "
            f"{source!r}; only {_SUPPORTED_SCHEME}:// is supported today"
        )
    if not parsed.netloc:
        raise ValueError(f"{source!r} has no bucket name")
    return parsed.netloc, parsed.path.lstrip("/")


def list_object_keys(bucket: str, prefix: str) -> list[str]:
    """List every object key under `prefix` in `bucket`.

    A real, paginated `list_objects_v2` call -- never assumes a single
    response (capped at 1000 keys per S3's own API contract) covers a
    real prefix at production volume.
    """
    client = boto3.client("s3")
    paginator = client.get_paginator("list_objects_v2")
    keys: list[str] = []
    for page in paginator.paginate(Bucket=bucket, Prefix=prefix):
        for obj in page.get("Contents", []):
            keys.append(obj["Key"])
    return keys


def iter_object_lines(bucket: str, key: str) -> Iterator[str]:
    """Yield each non-empty line of one object's body, decoded as text.

    Transparently gunzips a `.gz`-suffixed key -- the shipper config at
    docs/operations/vector-gatewayevents-s3.yaml writes gzip-compressed
    objects (`compression: gzip`). Every other key is read as plain UTF-8
    text, so this also works directly against a hand-written test/dev
    fixture with no compression at all.
    """
    client = boto3.client("s3")
    body = client.get_object(Bucket=bucket, Key=key)["Body"].read()
    if key.endswith(".gz"):
        body = gzip.decompress(body)
    for line in body.decode("utf-8").splitlines():
        if line.strip():
            yield line
