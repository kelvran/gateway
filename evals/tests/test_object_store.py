"""Unit tests for evals.ingestion.object_store.

Never hits a real AWS endpoint: `boto3.client` is monkeypatched to a small
in-memory fake that mimics exactly the two real boto3 calls this module
makes (`get_paginator("list_objects_v2").paginate(...)` and
`get_object(...)`), so these tests exercise this module's real pagination/
decompression logic without any network access or real credentials.
"""

from __future__ import annotations

import gzip

import pytest

from evals.ingestion import object_store


class _FakePaginator:
    def __init__(self, pages: list[dict]) -> None:
        self._pages = pages

    def paginate(self, Bucket: str, Prefix: str):  # noqa: N803 -- mirrors boto3's own kwarg casing
        return iter(self._pages)


class _FakeBody:
    def __init__(self, data: bytes) -> None:
        self._data = data

    def read(self) -> bytes:
        return self._data


class _FakeS3Client:
    def __init__(self, pages: list[dict], objects: dict[str, bytes]) -> None:
        self._pages = pages
        self._objects = objects

    def get_paginator(self, operation_name: str) -> _FakePaginator:
        assert operation_name == "list_objects_v2"
        return _FakePaginator(self._pages)

    def get_object(self, Bucket: str, Key: str) -> dict:  # noqa: N803
        return {"Body": _FakeBody(self._objects[Key])}


def test_parse_object_storage_uri_splits_bucket_and_prefix():
    bucket, prefix = object_store.parse_object_storage_uri(
        "s3://my-bucket/gatewayevents/v1/dt=2026-09-07/"
    )
    assert bucket == "my-bucket"
    assert prefix == "gatewayevents/v1/dt=2026-09-07/"


def test_parse_object_storage_uri_bucket_only_has_empty_prefix():
    bucket, prefix = object_store.parse_object_storage_uri("s3://my-bucket")
    assert bucket == "my-bucket"
    assert prefix == ""


def test_parse_object_storage_uri_rejects_non_s3_scheme():
    with pytest.raises(ValueError, match="unsupported object-storage scheme"):
        object_store.parse_object_storage_uri("gs://my-bucket/prefix")


def test_parse_object_storage_uri_rejects_missing_bucket():
    with pytest.raises(ValueError, match="no bucket name"):
        object_store.parse_object_storage_uri("s3:///prefix")


def test_list_object_keys_flattens_multiple_pages(monkeypatch):
    pages = [
        {"Contents": [{"Key": "a.jsonl"}, {"Key": "b.jsonl"}]},
        {"Contents": [{"Key": "c.jsonl"}]},
    ]
    fake_client = _FakeS3Client(pages, objects={})
    monkeypatch.setattr(object_store.boto3, "client", lambda service: fake_client)

    keys = object_store.list_object_keys("my-bucket", "gatewayevents/v1/")

    assert keys == ["a.jsonl", "b.jsonl", "c.jsonl"]


def test_list_object_keys_handles_a_page_with_no_contents_key(monkeypatch):
    # A real ListObjectsV2 response omits "Contents" entirely for an empty
    # prefix -- must not raise a KeyError.
    fake_client = _FakeS3Client([{}], objects={})
    monkeypatch.setattr(object_store.boto3, "client", lambda service: fake_client)

    assert object_store.list_object_keys("my-bucket", "empty-prefix/") == []


def test_iter_object_lines_reads_plain_text(monkeypatch):
    body = b'{"a": 1}\n{"b": 2}\n'
    fake_client = _FakeS3Client([], objects={"plain.jsonl": body})
    monkeypatch.setattr(object_store.boto3, "client", lambda service: fake_client)

    lines = list(object_store.iter_object_lines("my-bucket", "plain.jsonl"))

    assert lines == ['{"a": 1}', '{"b": 2}']


def test_iter_object_lines_transparently_gunzips_gz_suffixed_keys(monkeypatch):
    raw = b'{"a": 1}\n{"b": 2}\n'
    compressed = gzip.compress(raw)
    fake_client = _FakeS3Client([], objects={"compressed.jsonl.gz": compressed})
    monkeypatch.setattr(object_store.boto3, "client", lambda service: fake_client)

    lines = list(object_store.iter_object_lines("my-bucket", "compressed.jsonl.gz"))

    assert lines == ['{"a": 1}', '{"b": 2}']


def test_iter_object_lines_skips_blank_lines(monkeypatch):
    body = b'{"a": 1}\n\n   \n{"b": 2}\n'
    fake_client = _FakeS3Client([], objects={"with-blanks.jsonl": body})
    monkeypatch.setattr(object_store.boto3, "client", lambda service: fake_client)

    lines = list(object_store.iter_object_lines("my-bucket", "with-blanks.jsonl"))

    assert lines == ['{"a": 1}', '{"b": 2}']
