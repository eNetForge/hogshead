#!/usr/bin/env python3
"""boto3 smoke test for hogshead.

Exercises API features that command-line clients do not reach:
presigned PUT, conditional writes, range GET, flexible checksums,
URL-encoded listings, batch delete, bucket policy / CORS / lifecycle,
object tagging and multipart via UploadPartCopy.

Environment:
  S3_ENDPOINT            e.g. http://127.0.0.1:19100 (required)
  AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (required)
  AWS_DEFAULT_REGION     default us-east-1
  BOTO3_SMOKE_BUCKET     bucket name (default boto3-smoke-<pid>)

Prints one line per check ("ok <name>" or "FAIL <name>: <detail>") and
exits 1 if any check failed.  Needs awscrt for the CRC32C checks; they
are reported as SKIP without it.
"""
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError

ENDPOINT = os.environ.get("S3_ENDPOINT")
if not ENDPOINT:
    sys.exit("S3_ENDPOINT is required")
REGION = os.environ.get("AWS_DEFAULT_REGION", "us-east-1")
BUCKET = os.environ.get("BOTO3_SMOKE_BUCKET", f"boto3-smoke-{os.getpid()}")

try:
    from awscrt import checksums as crt_checksums
except ImportError:  # pragma: no cover
    crt_checksums = None

s3 = boto3.client(
    "s3",
    endpoint_url=ENDPOINT,
    region_name=REGION,
    config=Config(
        signature_version="s3v4",
        s3={"addressing_style": "path"},
        retries={"max_attempts": 2},
        request_checksum_calculation="when_supported",
        response_checksum_validation="when_supported",
    ),
)

RESULTS = []


def check(name):
    """Decorator: run the check, record ok/FAIL/SKIP, never raise."""

    def wrap(fn):
        def run():
            try:
                out = fn()
                status = "SKIP" if out == "skip" else "ok"
                RESULTS.append((status, name, ""))
                print(f"{status} {name}")
            except Exception as e:  # noqa: BLE001 - report every failure
                detail = _describe(e)
                RESULTS.append(("FAIL", name, detail))
                print(f"FAIL {name}: {detail}")

        run.__name__ = fn.__name__
        return run

    return wrap


def _describe(e):
    if isinstance(e, ClientError):
        meta = e.response.get("ResponseMetadata", {})
        err = e.response.get("Error", {})
        return (
            f"{e.operation_name} -> HTTP {meta.get('HTTPStatusCode')} "
            f"{err.get('Code')}: {err.get('Message')}"
        )
    return f"{type(e).__name__}: {e}"


def http(method, url, body=None, headers=None):
    req = urllib.request.Request(url, data=body, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()


def expect(cond, msg):
    if not cond:
        raise AssertionError(msg)


def expect_error(fn, code, status=None):
    try:
        fn()
    except ClientError as e:
        got = e.response["Error"]["Code"]
        got_status = e.response["ResponseMetadata"]["HTTPStatusCode"]
        expect(got == code, f"expected error {code}, got {got} (HTTP {got_status})")
        if status is not None:
            expect(got_status == status, f"expected HTTP {status} for {code}, got {got_status}")
        return
    raise AssertionError(f"expected {code} error, call succeeded")


# ---------------------------------------------------------------- checks

@check("create bucket")
def t_create():
    s3.create_bucket(Bucket=BUCKET)
    s3.head_bucket(Bucket=BUCKET)


@check("presigned PUT then GET")
def t_presigned_put():
    url = s3.generate_presigned_url(
        "put_object", Params={"Bucket": BUCKET, "Key": "presigned.txt"}, ExpiresIn=120
    )
    status, _, body = http("PUT", url, body=b"via presigned put")
    expect(status == 200, f"presigned PUT -> HTTP {status}: {body[:200]!r}")
    got = s3.get_object(Bucket=BUCKET, Key="presigned.txt")["Body"].read()
    expect(got == b"via presigned put", f"round trip mismatch: {got!r}")


@check("If-None-Match: * conditional put (second put -> 412)")
def t_if_none_match():
    s3.put_object(Bucket=BUCKET, Key="cond.txt", Body=b"first", IfNoneMatch="*")
    expect_error(
        lambda: s3.put_object(Bucket=BUCKET, Key="cond.txt", Body=b"second", IfNoneMatch="*"),
        "PreconditionFailed",
        412,
    )
    got = s3.get_object(Bucket=BUCKET, Key="cond.txt")["Body"].read()
    expect(got == b"first", f"object overwritten despite 412: {got!r}")


@check("range GET")
def t_range():
    s3.put_object(Bucket=BUCKET, Key="range.txt", Body=b"0123456789")
    r = s3.get_object(Bucket=BUCKET, Key="range.txt", Range="bytes=2-5")
    expect(r["ResponseMetadata"]["HTTPStatusCode"] == 206, "expected 206")
    expect(r["ContentRange"] == "bytes 2-5/10", f"Content-Range {r.get('ContentRange')}")
    body = r["Body"].read()
    expect(body == b"2345", f"body {body!r}")
    r = s3.get_object(Bucket=BUCKET, Key="range.txt", Range="bytes=-3")
    expect(r["Body"].read() == b"789", "suffix range")


@check("put with ChecksumAlgorithm=CRC32C, get with ChecksumMode=ENABLED")
def t_crc32c():
    if crt_checksums is None:
        return "skip"
    data = os.urandom(70000)
    want = base64.b64encode(crt_checksums.crc32c(data).to_bytes(4, "big")).decode()
    r = s3.put_object(Bucket=BUCKET, Key="crc32c.bin", Body=data, ChecksumAlgorithm="CRC32C")
    expect(r.get("ChecksumCRC32C") == want, f"PUT returned {r.get('ChecksumCRC32C')} want {want}")
    r = s3.get_object(Bucket=BUCKET, Key="crc32c.bin", ChecksumMode="ENABLED")
    expect(r.get("ChecksumCRC32C") == want, f"GET returned {r.get('ChecksumCRC32C')} want {want}")
    expect(r["Body"].read() == data, "body mismatch")  # botocore validates checksum here
    r = s3.head_object(Bucket=BUCKET, Key="crc32c.bin", ChecksumMode="ENABLED")
    expect(r.get("ChecksumCRC32C") == want, f"HEAD returned {r.get('ChecksumCRC32C')}")


@check("range GET on checksummed object does not carry mismatching full checksum")
def t_range_checksum():
    if crt_checksums is None:
        return "skip"
    data = os.urandom(70000)
    s3.put_object(Bucket=BUCKET, Key="crc32c-range.bin", Body=data, ChecksumAlgorithm="CRC32C")
    # botocore validates x-amz-checksum-* against the body it received; a
    # full-object checksum on a 206 response is therefore a client-visible bug.
    r = s3.get_object(Bucket=BUCKET, Key="crc32c-range.bin", ChecksumMode="ENABLED", Range="bytes=0-999")
    expect(r["Body"].read() == data[:1000], "range body mismatch")


@check("presigned GET with botocore default signer (SigV2 query auth)")
def t_presigned_sigv2():
    # botocore presigns with SigV2 unless signature_version is configured
    # explicitly; a default boto3.client() therefore produces this URL shape.
    legacy = boto3.client(
        "s3", endpoint_url=ENDPOINT, region_name=REGION,
        config=Config(s3={"addressing_style": "path"}),
    )
    s3.put_object(Bucket=BUCKET, Key="sigv2.txt", Body=b"v2")
    url = legacy.generate_presigned_url("get_object", Params={"Bucket": BUCKET, "Key": "sigv2.txt"}, ExpiresIn=120)
    expect("AWSAccessKeyId=" in url, f"botocore did not produce a SigV2 URL: {url}")
    status, _, body = http("GET", url)
    expect(status == 200, f"SigV2 presigned GET -> HTTP {status}: {body[:160]!r}")
    expect(body == b"v2", f"body {body!r}")


@check("user metadata keys are returned lower-case (x-amz-meta-*)")
def t_metadata_case():
    s3.put_object(Bucket=BUCKET, Key="meta-case.txt", Body=b"m", Metadata={"my-key": "1"})
    url = s3.generate_presigned_url("get_object", Params={"Bucket": BUCKET, "Key": "meta-case.txt"}, ExpiresIn=120)
    status, headers, _ = http("GET", url)
    names = [k for k in headers if k.lower() == "x-amz-meta-my-key"]
    expect(names == ["x-amz-meta-my-key"], f"header name on the wire is {names} (AWS sends lower-case)")
    h = s3.head_object(Bucket=BUCKET, Key="meta-case.txt")
    expect(h["Metadata"] == {"my-key": "1"}, f"boto3 Metadata {h['Metadata']}")


@check("list_objects_v2 with EncodingType=url (space and + in keys)")
def t_encoding():
    keys = ["enc/a b.txt", "enc/plus+plus.txt", "enc/pct%20.txt", "enc/uni-\u00fc\u00f1.txt"]
    encoded = ["enc/a%20b.txt", "enc/plus%2Bplus.txt", "enc/pct%2520.txt", "enc/uni-%C3%BC%C3%B1.txt"]
    for k in keys:
        s3.put_object(Bucket=BUCKET, Key=k, Body=b"x")
    # explicit EncodingType=url: botocore hands back the encoded keys untouched
    r = s3.list_objects_v2(Bucket=BUCKET, Prefix="enc/", EncodingType="url")
    expect(r.get("EncodingType") == "url", "EncodingType missing from v2 response")
    got = sorted(o["Key"] for o in r.get("Contents", []))
    expect(got == sorted(encoded), f"v2 encoded keys {got}")
    r = s3.list_objects(Bucket=BUCKET, Prefix="enc/", EncodingType="url", Delimiter="/")
    expect(r.get("EncodingType") == "url", "EncodingType missing from v1 response")
    got = sorted(o["Key"] for o in r.get("Contents", []))
    expect(got == sorted(encoded), f"v1 encoded keys {got}")
    # default call: botocore injects EncodingType=url itself and decodes the reply
    for fn in (s3.list_objects_v2, s3.list_objects):
        r = fn(Bucket=BUCKET, Prefix="enc/")
        got = sorted(o["Key"] for o in r.get("Contents", []))
        expect(got == sorted(keys), f"{fn.__name__} auto-decoded keys {got}")
    r = s3.list_object_versions(Bucket=BUCKET, Prefix="enc/")
    got = sorted(o["Key"] for o in r.get("Versions", []))
    expect(got == sorted(keys), f"list_object_versions auto-decoded keys {got}")


@check("delete_objects")
def t_delete_objects():
    keys = [f"del/{i}.txt" for i in range(5)]
    for k in keys:
        s3.put_object(Bucket=BUCKET, Key=k, Body=b"x")
    r = s3.delete_objects(Bucket=BUCKET, Delete={"Objects": [{"Key": k} for k in keys] + [{"Key": "del/missing"}]})
    deleted = sorted(d["Key"] for d in r.get("Deleted", []))
    expect(deleted == sorted(keys + ["del/missing"]), f"Deleted {deleted}")
    expect(not r.get("Errors"), f"Errors {r.get('Errors')}")
    r = s3.list_objects_v2(Bucket=BUCKET, Prefix="del/")
    expect(r.get("KeyCount", 0) == 0, "objects still listed after delete")


@check("bucket policy put/get/delete")
def t_policy():
    policy = {
        "Version": "2012-10-17",
        "Statement": [
            {
                "Sid": "PublicRead",
                "Effect": "Allow",
                "Principal": "*",
                "Action": ["s3:GetObject"],
                "Resource": [f"arn:aws:s3:::{BUCKET}/public/*"],
            }
        ],
    }
    s3.put_bucket_policy(Bucket=BUCKET, Policy=json.dumps(policy))
    got = json.loads(s3.get_bucket_policy(Bucket=BUCKET)["Policy"])
    expect(got["Statement"][0]["Sid"] == "PublicRead", f"policy round trip {got}")
    s3.put_object(Bucket=BUCKET, Key="public/p.txt", Body=b"pub")
    s3.put_object(Bucket=BUCKET, Key="private/p.txt", Body=b"priv")
    status, _, _ = http("GET", f"{ENDPOINT}/{BUCKET}/public/p.txt")
    expect(status == 200, f"anonymous GET of policy-public object -> {status}")
    status, _, _ = http("GET", f"{ENDPOINT}/{BUCKET}/private/p.txt")
    expect(status == 403, f"anonymous GET of private object -> {status}")
    s3.delete_bucket_policy(Bucket=BUCKET)
    expect_error(lambda: s3.get_bucket_policy(Bucket=BUCKET), "NoSuchBucketPolicy", 404)
    status, _, _ = http("GET", f"{ENDPOINT}/{BUCKET}/public/p.txt")
    expect(status == 403, f"anonymous GET after policy delete -> {status}")


@check("bucket CORS put/get/delete + preflight")
def t_cors():
    cfg = {
        "CORSRules": [
            {
                "AllowedOrigins": ["https://example.com"],
                "AllowedMethods": ["GET", "PUT"],
                "AllowedHeaders": ["*"],
                "ExposeHeaders": ["ETag"],
                "MaxAgeSeconds": 300,
            }
        ]
    }
    s3.put_bucket_cors(Bucket=BUCKET, CORSConfiguration=cfg)
    got = s3.get_bucket_cors(Bucket=BUCKET)["CORSRules"]
    expect(got[0]["AllowedOrigins"] == ["https://example.com"], f"CORS round trip {got}")
    expect(got[0]["MaxAgeSeconds"] == 300, f"MaxAgeSeconds {got}")
    status, headers, _ = http(
        "OPTIONS",
        f"{ENDPOINT}/{BUCKET}/anykey",
        headers={"Origin": "https://example.com", "Access-Control-Request-Method": "PUT"},
    )
    expect(status == 200, f"preflight -> {status}")
    h = {k.lower(): v for k, v in headers.items()}
    expect(h.get("access-control-allow-origin") == "https://example.com", f"preflight headers {h}")
    status, _, _ = http(
        "OPTIONS",
        f"{ENDPOINT}/{BUCKET}/anykey",
        headers={"Origin": "https://evil.example", "Access-Control-Request-Method": "PUT"},
    )
    expect(status == 403, f"preflight for disallowed origin -> {status}")
    s3.delete_bucket_cors(Bucket=BUCKET)
    expect_error(lambda: s3.get_bucket_cors(Bucket=BUCKET), "NoSuchCORSConfiguration", 404)


@check("lifecycle put/get/delete")
def t_lifecycle():
    cfg = {
        "Rules": [
            {
                "ID": "expire-tmp",
                "Filter": {"Prefix": "tmp/"},
                "Status": "Enabled",
                "Expiration": {"Days": 7},
            },
            {
                "ID": "abort-mpu",
                "Filter": {},
                "Status": "Enabled",
                "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 2},
                "NoncurrentVersionExpiration": {"NoncurrentDays": 30},
            },
        ]
    }
    s3.put_bucket_lifecycle_configuration(Bucket=BUCKET, LifecycleConfiguration=cfg)
    rules = s3.get_bucket_lifecycle_configuration(Bucket=BUCKET)["Rules"]
    ids = sorted(r["ID"] for r in rules)
    expect(ids == ["abort-mpu", "expire-tmp"], f"lifecycle round trip {rules}")
    r0 = next(r for r in rules if r["ID"] == "expire-tmp")
    expect(r0["Expiration"].get("Days") == 7, f"expiration days {r0}")
    expect(r0["Filter"].get("Prefix") == "tmp/", f"filter prefix {r0}")
    s3.delete_bucket_lifecycle(Bucket=BUCKET)
    expect_error(
        lambda: s3.get_bucket_lifecycle_configuration(Bucket=BUCKET),
        "NoSuchLifecycleConfiguration",
        404,
    )


@check("object tagging")
def t_tagging():
    s3.put_object(Bucket=BUCKET, Key="tagged.txt", Body=b"t", Tagging="env=test&team=s3")
    got = {t["Key"]: t["Value"] for t in s3.get_object_tagging(Bucket=BUCKET, Key="tagged.txt")["TagSet"]}
    expect(got == {"env": "test", "team": "s3"}, f"tags from PUT header {got}")
    h = s3.head_object(Bucket=BUCKET, Key="tagged.txt")
    expect(h.get("TagCount") == 2, f"x-amz-tagging-count {h.get('TagCount')}")
    s3.put_object_tagging(
        Bucket=BUCKET, Key="tagged.txt", Tagging={"TagSet": [{"Key": "only", "Value": "one"}]}
    )
    got = {t["Key"]: t["Value"] for t in s3.get_object_tagging(Bucket=BUCKET, Key="tagged.txt")["TagSet"]}
    expect(got == {"only": "one"}, f"tags after put_object_tagging {got}")
    s3.delete_object_tagging(Bucket=BUCKET, Key="tagged.txt")
    got = s3.get_object_tagging(Bucket=BUCKET, Key="tagged.txt")["TagSet"]
    expect(got == [], f"tags after delete {got}")


@check("multipart upload via upload_part_copy")
def t_upload_part_copy():
    src = os.urandom(6 * 1024 * 1024)
    s3.put_object(Bucket=BUCKET, Key="mpc-src.bin", Body=src)
    mp = s3.create_multipart_upload(Bucket=BUCKET, Key="mpc-dst.bin")
    uid = mp["UploadId"]
    parts = []
    p1 = s3.upload_part_copy(
        Bucket=BUCKET, Key="mpc-dst.bin", UploadId=uid, PartNumber=1,
        CopySource={"Bucket": BUCKET, "Key": "mpc-src.bin"}, CopySourceRange="bytes=0-5242879",
    )
    parts.append({"PartNumber": 1, "ETag": p1["CopyPartResult"]["ETag"]})
    p2 = s3.upload_part_copy(
        Bucket=BUCKET, Key="mpc-dst.bin", UploadId=uid, PartNumber=2,
        CopySource={"Bucket": BUCKET, "Key": "mpc-src.bin"}, CopySourceRange="bytes=5242880-6291455",
    )
    parts.append({"PartNumber": 2, "ETag": p2["CopyPartResult"]["ETag"]})
    listed = s3.list_parts(Bucket=BUCKET, Key="mpc-dst.bin", UploadId=uid)["Parts"]
    expect([p["PartNumber"] for p in listed] == [1, 2], f"list_parts {listed}")
    expect(listed[0]["Size"] == 5242880, f"part 1 size {listed[0]['Size']}")
    r = s3.complete_multipart_upload(
        Bucket=BUCKET, Key="mpc-dst.bin", UploadId=uid, MultipartUpload={"Parts": parts}
    )
    expect(r["ETag"].endswith('-2"'), f"multipart ETag {r['ETag']}")
    got = s3.get_object(Bucket=BUCKET, Key="mpc-dst.bin")["Body"].read()
    expect(got == src, "copied multipart content mismatch")


@check("list_multipart_uploads + abort")
def t_mpu_abort():
    mp = s3.create_multipart_upload(Bucket=BUCKET, Key="abort-me.bin")
    ups = s3.list_multipart_uploads(Bucket=BUCKET).get("Uploads", [])
    expect(any(u["UploadId"] == mp["UploadId"] for u in ups), f"upload not listed {ups}")
    s3.abort_multipart_upload(Bucket=BUCKET, Key="abort-me.bin", UploadId=mp["UploadId"])
    expect_error(
        lambda: s3.list_parts(Bucket=BUCKET, Key="abort-me.bin", UploadId=mp["UploadId"]),
        "NoSuchUpload",
        404,
    )


@check("copy_object with MetadataDirective=REPLACE and content-type")
def t_copy_metadata():
    s3.put_object(Bucket=BUCKET, Key="meta-src.txt", Body=b"m", Metadata={"a": "1"}, ContentType="text/plain")
    s3.copy_object(
        Bucket=BUCKET, Key="meta-dst.txt", CopySource={"Bucket": BUCKET, "Key": "meta-src.txt"},
        MetadataDirective="REPLACE", Metadata={"b": "2"}, ContentType="application/json",
    )
    h = s3.head_object(Bucket=BUCKET, Key="meta-dst.txt")
    expect({k.lower(): v for k, v in h["Metadata"].items()} == {"b": "2"}, f"metadata {h['Metadata']}")
    expect(h["ContentType"] == "application/json", f"content type {h['ContentType']}")
    s3.copy_object(Bucket=BUCKET, Key="meta-dst2.txt", CopySource={"Bucket": BUCKET, "Key": "meta-src.txt"})
    h = s3.head_object(Bucket=BUCKET, Key="meta-dst2.txt")
    expect({k.lower(): v for k, v in h["Metadata"].items()} == {"a": "1"}, f"COPY directive metadata {h['Metadata']}")


@check("cleanup: delete all objects and bucket")
def t_cleanup():
    paginator = s3.get_paginator("list_object_versions")
    for page in paginator.paginate(Bucket=BUCKET):
        objs = [{"Key": o["Key"], "VersionId": o["VersionId"]} for o in page.get("Versions", [])]
        objs += [{"Key": o["Key"], "VersionId": o["VersionId"]} for o in page.get("DeleteMarkers", [])]
        if objs:
            s3.delete_objects(Bucket=BUCKET, Delete={"Objects": objs, "Quiet": True})
    for u in s3.list_multipart_uploads(Bucket=BUCKET).get("Uploads", []):
        s3.abort_multipart_upload(Bucket=BUCKET, Key=u["Key"], UploadId=u["UploadId"])
    s3.delete_bucket(Bucket=BUCKET)
    expect_error(lambda: s3.head_bucket(Bucket=BUCKET), "404", 404)


def main():
    checks = [
        t_create, t_presigned_put, t_presigned_sigv2, t_if_none_match, t_range, t_crc32c,
        t_range_checksum, t_metadata_case, t_encoding, t_delete_objects, t_policy, t_cors, t_lifecycle, t_tagging,
        t_upload_part_copy, t_mpu_abort, t_copy_metadata, t_cleanup,
    ]
    start = time.time()
    for c in checks:
        c()
    failed = [r for r in RESULTS if r[0] == "FAIL"]
    skipped = [r for r in RESULTS if r[0] == "SKIP"]
    print(
        f"boto3 smoke: {len(RESULTS) - len(failed) - len(skipped)} ok, "
        f"{len(failed)} failed, {len(skipped)} skipped in {time.time() - start:.1f}s"
    )
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
