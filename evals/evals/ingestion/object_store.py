"""Lists/reads gatewayevents_v1 objects from object storage.

Per docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md: S3 was
the first cloud shipped here. Per that RFC's own 2026-09-07 addendum, a
`gs://` (GCS) sibling path is now real too -- `parse_object_storage_uri`
dispatches on scheme, and `list_object_keys`/`iter_object_lines` each
route internally to a `boto3`- or `google-cloud-storage`-backed helper
for the same (bucket, prefix)/(bucket, key) shape, so callers (namely
evals.cli's `ingest` command) never need their own cloud-specific
branching. This module's job stops at raw bytes/lines -- it never decodes
a GatewayDecisionEvent itself. That stays evals.ingestion.decode's exact
job, called separately by whoever consumes this module (see evals.cli's
`ingest` command), per this project's own `api/gatewayevents`
"reuse, don't duplicate" contract discipline.
"""

from __future__ import annotations

import gzip
from collections.abc import Iterator
from urllib.parse import urlsplit

import boto3
from google.cloud import storage as gcs_storage

_SUPPORTED_SCHEMES = frozenset({"s3", "gs"})


def parse_object_storage_uri(source: str) -> tuple[str, str, str]:
    """Split an `s3://bucket/prefix` or `gs://bucket/prefix` source into
    `(scheme, bucket, prefix)`.

    `prefix` is `""` when `source` names a bucket with no path at all
    (e.g. `s3://bucket`) -- a real, valid "list everything in the bucket"
    request, not an error. Any scheme other than `s3://`/`gs://` raises a
    clear `ValueError` rather than silently misbehaving. `scheme` is
    returned (not just validated) so callers -- `list_object_keys`/
    `iter_object_lines` below, and evals.cli's `ingest` command -- can
    route to the right cloud backend without re-parsing `source`
    themselves or hand-rolling their own scheme check.
    """
    parsed = urlsplit(source)
    if parsed.scheme not in _SUPPORTED_SCHEMES:
        supported = "/".join(f"{s}://" for s in sorted(_SUPPORTED_SCHEMES))
        raise ValueError(
            f"unsupported object-storage scheme {parsed.scheme!r} in "
            f"{source!r}; only {supported} is supported today"
        )
    if not parsed.netloc:
        raise ValueError(f"{source!r} has no bucket name")
    return parsed.scheme, parsed.netloc, parsed.path.lstrip("/")


def list_object_keys(scheme: str, bucket: str, prefix: str) -> list[str]:
    """List every object key under `prefix` in `bucket`, for `scheme`
    (`"s3"` or `"gs"`, as returned by `parse_object_storage_uri`).

    S3: a real, paginated `list_objects_v2` call -- never assumes a
    single response (capped at 1000 keys per S3's own API contract)
    covers a real prefix at production volume. GCS: `Client.list_blobs`
    returns a real, auto-paginating iterator over every matching blob --
    no manual page-token loop needed, `google-cloud-storage` already
    exhausts every page as the iterator is consumed.
    """
    if scheme == "s3":
        return _list_object_keys_s3(bucket, prefix)
    if scheme == "gs":
        return _list_object_keys_gcs(bucket, prefix)
    raise ValueError(f"unsupported object-storage scheme {scheme!r}")


def _list_object_keys_s3(bucket: str, prefix: str) -> list[str]:
    client = boto3.client("s3")
    paginator = client.get_paginator("list_objects_v2")
    keys: list[str] = []
    for page in paginator.paginate(Bucket=bucket, Prefix=prefix):
        for obj in page.get("Contents", []):
            keys.append(obj["Key"])
    return keys


def _list_object_keys_gcs(bucket: str, prefix: str) -> list[str]:
    client = gcs_storage.Client()
    return [blob.name for blob in client.list_blobs(bucket, prefix=prefix)]


def iter_object_lines(scheme: str, bucket: str, key: str) -> Iterator[str]:
    """Yield each non-empty line of one object's body, decoded as text,
    for `scheme` (`"s3"` or `"gs"`, as returned by
    `parse_object_storage_uri`).

    Transparently gunzips a `.gz`-suffixed key -- the shipper configs at
    docs/operations/vector-gatewayevents-s3.yaml and
    docs/operations/vector-gatewayevents-gcs.yaml both write
    gzip-compressed objects (`compression: gzip`). Every other key is
    read as plain UTF-8 text, so this also works directly against a
    hand-written test/dev fixture with no compression at all. The
    gzip/line-splitting logic itself is shared across both clouds --
    only the byte-fetch step differs.
    """
    if scheme == "s3":
        body = _get_object_bytes_s3(bucket, key)
    elif scheme == "gs":
        body = _get_object_bytes_gcs(bucket, key)
    else:
        raise ValueError(f"unsupported object-storage scheme {scheme!r}")
    if key.endswith(".gz"):
        body = gzip.decompress(body)
    for line in body.decode("utf-8").splitlines():
        if line.strip():
            yield line


def _get_object_bytes_s3(bucket: str, key: str) -> bytes:
    client = boto3.client("s3")
    return client.get_object(Bucket=bucket, Key=key)["Body"].read()


def _get_object_bytes_gcs(bucket: str, key: str) -> bytes:
    client = gcs_storage.Client()
    return client.bucket(bucket).blob(key).download_as_bytes()
