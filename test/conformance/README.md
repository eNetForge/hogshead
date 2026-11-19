# ceph/s3-tests conformance

`run.sh` runs the [ceph/s3-tests](https://github.com/ceph/s3-tests) boto3
functional suite (`s3tests/functional/test_s3.py`) against a throwaway
hogshead server.

```sh
go build -o bin/hogshead ./cmd/hogshead

# default: run exactly the tests in allowlist.txt; fails if any of them fails
test/conformance/run.sh

# broad run over the areas we care about; prints tests that newly pass
test/conformance/run.sh --discover
test/conformance/run.sh --discover --write-allowlist   # ...and refresh allowlist.txt

# anything else (server + config are still set up for you)
test/conformance/run.sh -- -k test_bucket_list_empty --tb=long
```

On first use the script shallow-clones s3-tests and builds a venv from its
`requirements.txt` (falling back to tox if that fails), plus `pytest-timeout`
and `pytest-xdist`; point `S3TESTS_DIR` / `S3TESTS_VENV` at existing ones to
skip that. It then runs `quickstart`, starts `serve` (Raft port = `PORT`+300),
creates two more users (`alt`, `tenant`) with the admin CLI, renders
`s3tests.conf.template`, and runs pytest with `-q -p no:cacheprovider
--timeout 30 -n 4`. Results (`junit.xml`, `passing.txt`, `failing.txt`,
`newly-passing.txt`, `regressions.txt`) land in `OUT_DIR`
(default `$SCRATCH/conformance-results`). All knobs are listed at the top of
`run.sh`.

`hogshead_s3tests_plugin.py` is loaded with `-p`; it makes the suite's bucket
sweep abort in-progress multipart uploads first, because hogshead refuses
`DeleteBucket` while uploads are in progress (AWS does not) and without the
workaround one leftover upload cascades into setup errors for every later
test in the same worker. `S3TESTS_ABORT_MPU_IN_NUKE=0` disables it.

## What `--discover` runs

Markers excluded (`-m`), i.e. features hogshead does not implement or tests
that fail on AWS itself:

```
fails_on_aws lifecycle_expiration lifecycle_transition sse_s3 encryption
bucket_encryption s3select s3website cloud_transition cloud_restore
test_of_sts iam_tenant iam_account iam_role iam_user iam_cross_account
webidentity_test fails_strict_rfc2616 bucket_logging checksum storage_class
object_lock sns s3control appendobject abac_test
```

Node-id keywords included (`-k`): bucket list/create/delete/head/location,
object put/get/head/delete, list_objects v1+v2 (delimiter, prefix, marker,
max-keys, encoding), multipart (upload, copy, list, abort, complete, etag),
versioning (enable/suspend, versions, delete markers), conditional writes,
ranges, copy (metadata directive, conditions), delete_objects, bucket policy,
CORS, lifecycle set/get/delete, presigned URLs, canned ACLs, tagging.
Override with `S3TESTS_MARKERS` / `S3TESTS_KEYWORDS`.

## Results (s3-tests 5522d1c, 2026-05-27; hogshead built from this tree)

Broad run: **436 tests run, 254 passed, 182 failed, 1 skipped** (about 50 s
with 4 workers). The passing node ids are in `allowlist.txt`, minus three
tagging tests (`test_put_delete_tags`, `test_put_modify_tags`,
`test_set_multipart_tagging`) that compare multi-tag sets in order and only
pass by luck because the server returns tags in random order (see REPORT.md);
the default mode re-runs the remaining 251 and passes 251/251.

Top failure categories (details and reproductions in `../REPORT.md`):

| count | category |
|------:|----------|
| 35 | `POST Object` (browser form upload) is not implemented: 501 where tests expect 400/204/201/200 |
| 27 | no per-owner isolation: `alt` user can read/write/copy into `main` user's private buckets (`ClientError not raised`); `Deny`-free policy tests that expect denial |
| 16 | ACL grants are not reflected/enforced: `get_bucket_acl`/`get_object_acl` only ever show the owner (`assert 1 == 2/3`), public-read does not grant list, public-read-write does not grant write, object ACLs ignored |
| 14 | bucket policy `Condition` not supported (`MalformedPolicy`), `Action: "*"` rejected, `GetBucketPolicyStatus` unsupported |
| 11 | user metadata keys returned Go-canonicalised (`Foo`, `Key1`) instead of lower-case (`KeyError: 'meta1'`, dict mismatches) |
| 9 | ranged GET returns the full-object checksum header (`FlexibleChecksumError`); includes multipart copy tests whose final GET is ranged |
| 8 | presigned URL edge cases: SigV2 query auth rejected (`Unsupported Authorization type`), `X-Amz-Expires` out of range gives 400 (tests expect 403), presigned CORS tests built on SigV2 |
| 8 | not implemented and not excluded by marker: ownership controls, public access block, object lock default retention, torrent |
| 6 | multipart: second `CompleteMultipartUpload` on a finished upload gives 404 `NoSuchUpload` (tests expect idempotent 200), re-sent part keeps the old ETag (`InvalidPart`), out-of-range `partNumber` gives 416 not 400, `UploadPartCopy` beyond source accepted |
| 5 | `CreateBucket` on an existing own bucket returns 409 (`us-east-1` semantics expect 200), and 409 `BucketAlreadyOwnedByYou` is also returned when *another* user owns it (expect `BucketAlreadyExists`) |
| 5 | listing details: `max-keys=0` sets `IsTruncated=true`; `<Marker>` / `<ContinuationToken>` not echoed; v1 `<Prefix>` is URL-encoded |
| 4 | tagging: tag order is random per request (Go map iteration), so order-sensitive tests are flaky; `GetObjectAttributes` returns `ObjectParts` for single-part objects / no `PartsCount` for multipart |
| rest | `Transfer-Encoding: chunked` without Content-Length -> 411; `Content-Encoding: aws-chunked` stored; anonymous requests to a missing bucket get 403 not 404; lifecycle rules without ID / with bad dates accepted; `If-Match` on missing key gives 412 not 404; 3 tests broken by newer botocore (`StreamingChecksumBody` TypeError) |
