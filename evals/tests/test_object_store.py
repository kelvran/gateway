"""Unit tests for evals.ingestion.object_store.

Never hits a real AWS or GCP endpoint: `boto3.client`/`google.cloud.storage`
are monkeypatched to small in-memory fakes that mimic exactly the real
calls this module makes for each cloud (`get_paginator("list_objects_v2")
.paginate(...)` + `get_object(...)` for S3; `list_blobs(...)` +
`bucket(...).blob(...).download_as_bytes()` for GCS), so these tests
exercise this module's real scheme-dispatch/pagination/decompression logic
without any network access or real credentials.
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


class _FakeBlob:
    def __init__(self, name: str, objects: dict[str, bytes] | None = None) -> None:
        self.name = name
        self._objects = objects

    def download_as_bytes(self) -> bytes:
        return self._objects[self.name]


class _FakeGcsBucket:
    def __init__(self, objects: dict[str, bytes]) -> None:
        self._objects = objects

    def blob(self, name: str) -> _FakeBlob:
        return _FakeBlob(name, self._objects)


class _FakeGcsClient:
    def __init__(self, keys: list[str], objects: dict[str, bytes]) -> None:
        self._keys = keys
        self._objects = objects

    def list_blobs(self, bucket_or_name: str, prefix: str):
        return iter(_FakeBlob(key) for key in self._keys)

    def bucket(self, bucket_name: str) -> _FakeGcsBucket:
        return _FakeGcsBucket(self._objects)


def test_parse_object_storage_uri_splits_scheme_bucket_and_prefix():
    scheme, bucket, prefix = object_store.parse_object_storage_uri(
        "s3://my-bucket/gatewayevents/v1/dt=2026-09-07/"
    )
    assert scheme == "s3"
    assert bucket == "my-bucket"
    assert prefix == "gatewayevents/v1/dt=2026-09-07/"


def test_parse_object_storage_uri_supports_gs_scheme():
    scheme, bucket, prefix = object_store.parse_object_storage_uri(
        "gs://my-bucket/gatewayevents/v1/dt=2026-09-07/"
    )
    assert scheme == "gs"
    assert bucket == "my-bucket"
    assert prefix == "gatewayevents/v1/dt=2026-09-07/"


def test_parse_object_storage_uri_bucket_only_has_empty_prefix():
    scheme, bucket, prefix = object_store.parse_object_storage_uri("s3://my-bucket")
    assert scheme == "s3"
    assert bucket == "my-bucket"
    assert prefix == ""


def test_parse_object_storage_uri_rejects_unsupported_scheme():
    with pytest.raises(ValueError, match="unsupported object-storage scheme"):
        object_store.parse_object_storage_uri("ftp://my-bucket/prefix")


def test_parse_object_storage_uri_rejects_missing_bucket():
    with pytest.raises(ValueError, match="no bucket name"):
        object_store.parse_object_storage_uri("s3:///prefix")


def test_list_object_keys_flattens_multiple_pages_s3(monkeypatch):
    pages = [
        {"Contents": [{"Key": "a.jsonl"}, {"Key": "b.jsonl"}]},
        {"Contents": [{"Key": "c.jsonl"}]},
    ]
    fake_client = _FakeS3Client(pages, objects={})
    monkeypatch.setattr(
        object_store.boto3, "client", lambda service, region_name=None: fake_client
    )

    keys = object_store.list_object_keys("s3", "my-bucket", "gatewayevents/v1/")

    assert keys == ["a.jsonl", "b.jsonl", "c.jsonl"]


def test_list_object_keys_handles_a_page_with_no_contents_key_s3(monkeypatch):
    # A real ListObjectsV2 response omits "Contents" entirely for an empty
    # prefix -- must not raise a KeyError.
    fake_client = _FakeS3Client([{}], objects={})
    monkeypatch.setattr(
        object_store.boto3, "client", lambda service, region_name=None: fake_client
    )

    assert object_store.list_object_keys("s3", "my-bucket", "empty-prefix/") == []


def test_list_object_keys_lists_every_blob_gs(monkeypatch):
    fake_client = _FakeGcsClient(["a.jsonl", "b.jsonl", "c.jsonl"], objects={})
    monkeypatch.setattr(object_store.gcs_storage, "Client", lambda: fake_client)

    keys = object_store.list_object_keys("gs", "my-bucket", "gatewayevents/v1/")

    assert keys == ["a.jsonl", "b.jsonl", "c.jsonl"]


def test_list_object_keys_handles_no_matching_blobs_gs(monkeypatch):
    fake_client = _FakeGcsClient([], objects={})
    monkeypatch.setattr(object_store.gcs_storage, "Client", lambda: fake_client)

    assert object_store.list_object_keys("gs", "my-bucket", "empty-prefix/") == []


def test_list_object_keys_rejects_unsupported_scheme():
    with pytest.raises(ValueError, match="unsupported object-storage scheme"):
        object_store.list_object_keys("ftp", "my-bucket", "prefix/")


def test_list_object_keys_resolves_region_from_aws_region_env_var_s3(monkeypatch):
    # Real bug (mirrors evals.judge.providers.make_bedrock_call_model's own
    # 2026-09-08 fix, per DECISIONS.md): this project's pinned botocore only
    # honors AWS_DEFAULT_REGION for a bare boto3.client(region_name=None)
    # call, NOT AWS_REGION alone. An operator who sets only AWS_REGION (per
    # evals/.env.example) must not hit botocore.exceptions.NoRegionError --
    # list_object_keys("s3", ...) must resolve AWS_REGION itself and pass a
    # real value to boto3.client, never leave region_name=None for
    # botocore's own (unreliable, for this pinned version) env-var scan.
    monkeypatch.delenv("AWS_DEFAULT_REGION", raising=False)
    monkeypatch.setenv("AWS_REGION", "eu-west-1")
    fake_client = _FakeS3Client([{}], objects={})
    captured = {}

    def fake_boto3_client(service, region_name=None):
        captured["region_name"] = region_name
        return fake_client

    monkeypatch.setattr(object_store.boto3, "client", fake_boto3_client)

    object_store.list_object_keys("s3", "my-bucket", "prefix/")

    assert captured["region_name"] == "eu-west-1"


def test_list_object_keys_falls_back_to_aws_default_region_env_var_s3(monkeypatch):
    monkeypatch.delenv("AWS_REGION", raising=False)
    monkeypatch.setenv("AWS_DEFAULT_REGION", "ap-southeast-1")
    fake_client = _FakeS3Client([{}], objects={})
    captured = {}

    def fake_boto3_client(service, region_name=None):
        captured["region_name"] = region_name
        return fake_client

    monkeypatch.setattr(object_store.boto3, "client", fake_boto3_client)

    object_store.list_object_keys("s3", "my-bucket", "prefix/")

    assert captured["region_name"] == "ap-southeast-1"


def test_iter_object_lines_reads_plain_text_s3(monkeypatch):
    body = b'{"a": 1}\n{"b": 2}\n'
    fake_client = _FakeS3Client([], objects={"plain.jsonl": body})
    monkeypatch.setattr(
        object_store.boto3, "client", lambda service, region_name=None: fake_client
    )

    lines = list(object_store.iter_object_lines("s3", "my-bucket", "plain.jsonl"))

    assert lines == ['{"a": 1}', '{"b": 2}']


def test_iter_object_lines_transparently_gunzips_gz_suffixed_keys_s3(monkeypatch):
    raw = b'{"a": 1}\n{"b": 2}\n'
    compressed = gzip.compress(raw)
    fake_client = _FakeS3Client([], objects={"compressed.jsonl.gz": compressed})
    monkeypatch.setattr(
        object_store.boto3, "client", lambda service, region_name=None: fake_client
    )

    lines = list(
        object_store.iter_object_lines("s3", "my-bucket", "compressed.jsonl.gz")
    )

    assert lines == ['{"a": 1}', '{"b": 2}']


def test_iter_object_lines_skips_blank_lines_s3(monkeypatch):
    body = b'{"a": 1}\n\n   \n{"b": 2}\n'
    fake_client = _FakeS3Client([], objects={"with-blanks.jsonl": body})
    monkeypatch.setattr(
        object_store.boto3, "client", lambda service, region_name=None: fake_client
    )

    lines = list(object_store.iter_object_lines("s3", "my-bucket", "with-blanks.jsonl"))

    assert lines == ['{"a": 1}', '{"b": 2}']


def test_iter_object_lines_resolves_region_from_aws_region_env_var_s3(monkeypatch):
    # Same real bug as list_object_keys's own region-resolution test above,
    # for _get_object_bytes_s3's boto3.client("s3") call.
    monkeypatch.delenv("AWS_DEFAULT_REGION", raising=False)
    monkeypatch.setenv("AWS_REGION", "eu-west-1")
    fake_client = _FakeS3Client([], objects={"plain.jsonl": b'{"a": 1}\n'})
    captured = {}

    def fake_boto3_client(service, region_name=None):
        captured["region_name"] = region_name
        return fake_client

    monkeypatch.setattr(object_store.boto3, "client", fake_boto3_client)

    list(object_store.iter_object_lines("s3", "my-bucket", "plain.jsonl"))

    assert captured["region_name"] == "eu-west-1"


def test_iter_object_lines_falls_back_to_aws_default_region_env_var_s3(monkeypatch):
    monkeypatch.delenv("AWS_REGION", raising=False)
    monkeypatch.setenv("AWS_DEFAULT_REGION", "ap-southeast-1")
    fake_client = _FakeS3Client([], objects={"plain.jsonl": b'{"a": 1}\n'})
    captured = {}

    def fake_boto3_client(service, region_name=None):
        captured["region_name"] = region_name
        return fake_client

    monkeypatch.setattr(object_store.boto3, "client", fake_boto3_client)

    list(object_store.iter_object_lines("s3", "my-bucket", "plain.jsonl"))

    assert captured["region_name"] == "ap-southeast-1"


def test_iter_object_lines_reads_plain_text_gs(monkeypatch):
    body = b'{"a": 1}\n{"b": 2}\n'
    fake_client = _FakeGcsClient([], objects={"plain.jsonl": body})
    monkeypatch.setattr(object_store.gcs_storage, "Client", lambda: fake_client)

    lines = list(object_store.iter_object_lines("gs", "my-bucket", "plain.jsonl"))

    assert lines == ['{"a": 1}', '{"b": 2}']


def test_iter_object_lines_transparently_gunzips_gz_suffixed_keys_gs(monkeypatch):
    raw = b'{"a": 1}\n{"b": 2}\n'
    compressed = gzip.compress(raw)
    fake_client = _FakeGcsClient([], objects={"compressed.jsonl.gz": compressed})
    monkeypatch.setattr(object_store.gcs_storage, "Client", lambda: fake_client)

    lines = list(
        object_store.iter_object_lines("gs", "my-bucket", "compressed.jsonl.gz")
    )

    assert lines == ['{"a": 1}', '{"b": 2}']


def test_iter_object_lines_skips_blank_lines_gs(monkeypatch):
    body = b'{"a": 1}\n\n   \n{"b": 2}\n'
    fake_client = _FakeGcsClient([], objects={"with-blanks.jsonl": body})
    monkeypatch.setattr(object_store.gcs_storage, "Client", lambda: fake_client)

    lines = list(object_store.iter_object_lines("gs", "my-bucket", "with-blanks.jsonl"))

    assert lines == ['{"a": 1}', '{"b": 2}']


def test_iter_object_lines_rejects_unsupported_scheme():
    with pytest.raises(ValueError, match="unsupported object-storage scheme"):
        list(object_store.iter_object_lines("ftp", "my-bucket", "key.jsonl"))
