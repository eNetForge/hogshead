# hogshead external-client compatibility report

> **Status after fixes (2026-10-09, later the same night):** A1 to A5, B1 to B9
> and the tag-order issue in C were fixed in the server. The client matrix
> now passes for aws cli v2, s3cmd, rclone, restic and boto3 (Velero still
> needs a Kubernetes cluster), and the 251-test conformance allowlist passes;
> a discovery run was started to extend the allowlist. Remaining open items
> are in section D (POST Object form uploads, per-grant ACLs, ownership
> controls, bucket-level default retention). The text below is the original
> report and is kept as the record of what was observed.


Server: this tree as of 2026-10-09 00:44 (`go build -o bin/hogshead
./cmd/hogshead`; files under `internal/` were being edited concurrently while
these runs happened, so re-run both harnesses after rebuilding), run via
`quickstart` + `serve`, region `us-east-1`, path-style, plain HTTP on
127.0.0.1. Clients: aws cli 2.37.11 (and 1.46.1), s3cmd 2.4.0, rclone 1.74.3,
restic 0.19.1, boto3 1.43.110 / botocore 1.43.110 (+awscrt), ceph/s3-tests
5522d1c (2026-05-27). Harness: `test/clients/run.sh`, `test/conformance/run.sh`.

Everything below was reproduced against a fresh server with the exact requests
shown. "Expected" means what AWS S3 does (and what the client relies on).

## Summary

| suite | result | caused by |
|---|---|---|
| aws cli v2 | FAIL (2/21 steps) | A1 |
| s3cmd | FAIL (2/17 steps) | A4 |
| rclone | PASS (8/8) | |
| restic | PASS (8/8) | |
| boto3 smoke | FAIL (4/18 checks) | A1, A2, A5 |
| velero | SKIP (needs Kubernetes; manual procedure in `clients/README.md`) | |
| s3-tests broad subset | 254 passed / 182 failed / 1 skipped of 436 | see `conformance/README.md` |

---

## A. Breaks a client

### A1. Ranged GET (206) carries the *full-object* checksum header

Any object that has a checksum (boto3 >= 1.36 and aws cli v2 attach one to
every upload by default) returns its whole-object `x-amz-checksum-*` on a
partial response. Clients validate the header against the bytes they got and
abort.

Reproduction (object uploaded as 2 x 8 MiB parts with CRC64NVME, but a 70 kB
single PUT with CRC32 behaves the same):

```
GET /cksum/mp HTTP/1.1
Range: bytes=0-8388607
x-amz-checksum-mode: ENABLED

HTTP/1.1 206 Partial Content
Content-Length: 8388608
Content-Range: bytes 0-8388607/12582912
Etag: "142ca8dbbcd8ebe65636bb7853335f23-2"
X-Amz-Checksum-Crc64nvme: VhaRHb9fs+g=        <- checksum of all 12582912 bytes
X-Amz-Checksum-Type: FULL_OBJECT
X-Amz-Mp-Parts-Count: 2
```

Same for `Range: bytes=0-99` and for the last part. Expected: no
`x-amz-checksum-*` header on a 206 response (AWS only returns a checksum for a
range when it is a `partNumber` / part-aligned request on a COMPOSITE
multipart object, and then it is the *part's* checksum).

Impact observed:

- aws cli v2: `aws s3 cp s3://b/big.bin ./big.bin` (12 MiB, downloaded as
  ranged parts) -> `download failed: ... Expected checksum PKaGL/Jwpwo= did
  not match calculated checksum: ZvTo0Vo0xic=`. `aws s3 sync` down fails the
  same way for every file >= 8 MiB.
- boto3 with default config: `s3.get_object(Bucket=b, Key=k, Range='bytes=2-5')`
  on an object it uploaded itself -> `FlexibleChecksumError` (botocore sends
  `x-amz-checksum-mode: ENABLED` on every GetObject by default).
- s3-tests: 9 failures (`test_ranged_request_*`, `test_multipart_copy_*`).

### A2. SigV2 (query-string and header) authentication is rejected with 400

```
GET /b/k?AWSAccessKeyId=MNONHXNELFLTWONK3UMW&Signature=t3iXXy%2B2fx%2BnJ4FANB75UnGOf%2Bg%3D&Expires=1791503680 HTTP/1.1

HTTP/1.1 400 Bad Request
<Error><Code>InvalidArgument</Code><Message>Unsupported Authorization type</Message>...
```

Expected: 200 (AWS in us-east-1 still honours SigV2 URLs). botocore's
`generate_presigned_url()` emits exactly this URL shape unless the client was
built with `Config(signature_version='s3v4')`, so every boto3 user who follows
the common presign recipe gets 400s. (SigV4 presigned GET and PUT work.)
s3-tests: 4 failures (`test_cors_presigned_*_v2`); `s3cmd` with
`signature_v2 = True` would fail the same way.

If SigV2 is intentionally unsupported, 403 `AccessDenied`/`SignatureDoesNotMatch`
would at least be the error class clients expect; 400 `InvalidArgument` is
treated as a programming error.

### A3. DeleteBucket refuses buckets with in-progress multipart uploads

```
POST /b/x?uploads            -> 200 (upload left open)
DELETE /b

HTTP/1.1 409 Conflict
<Error><Code>BucketNotEmpty</Code><Message>bucket "b" has in-progress multipart uploads</Message>
```

Expected: 204, uploads discarded. AWS only refuses this for *directory*
buckets (documented in the DeleteBucket API note); general-purpose buckets
delete fine, and cleanup tooling (`aws s3 rb --force`, Terraform destroy,
s3-tests' per-test bucket sweep) relies on it. In s3-tests this single
behaviour cascaded into 184 setup errors (every test after the first one that
leaves an upload open), so the harness patches the sweep
(`conformance/hogshead_s3tests_plugin.py`) to abort uploads first.

### A4. ACLs are silently accepted and ignored (except one canned bucket ACL)

All of these return 200 and change nothing:

```
PUT /b/k?acl              x-amz-acl: public-read                   -> 200, anon GET /b/k -> 403
PUT /b/k?acl              <AccessControlPolicy> with AllUsers READ  -> 200, anon GET /b/k -> 403
PUT /b?acl                <AccessControlPolicy> with AllUsers READ  -> 200, anon GET /b/k -> 403
PUT /b?acl                x-amz-grant-read: uri=.../global/AllUsers -> 200, anon GET /b/k -> 403
GET /b?acl / GET /b/k?acl  after any of the above                   -> only the owner FULL_CONTROL grant
```

Only `PUT /b?acl` with header `x-amz-acl: public-read` (and `create-bucket
--acl public-read`) has an effect: it installs a bucket policy
(`{"Sid":"PublicRead","Principal":"*","Action":["s3:GetObject","s3:GetObjectVersion"]}`)
and anonymous GET works. But:

- it does not grant `s3:ListBucket` (AWS `public-read` does): anonymous
  `GET /b?list-type=2` -> 403 (s3-tests `test_bucket_list_objects_anonymous`,
  `test_cors_origin_*`).
- `x-amz-acl: public-read-write` behaves like `public-read`: anonymous
  `PUT /b/anon.txt` -> 403 (expected 200).
- `GET /b?acl` still shows only the owner (expected an AllUsers READ grant).

Impact: `s3cmd setacl --acl-public s3://b/k` and `s3cmd setacl --acl-public
s3://b` (both send the XML form) report success but the object stays private;
s3-tests: 16 failures in `test_*_acl_canned*`, `test_object_raw_get_*`,
`test_object_anon_put_write_access`. If per-grant ACLs are out of scope,
returning 501 `NotImplemented` for ACL bodies / `x-amz-grant-*` instead of 200
would stop clients from believing the change took.

### A5. User metadata header names are returned Go-canonicalised

```
PUT /b/m1          x-amz-meta-my-key: 2
GET /b/m1

HTTP/1.1 200 OK
X-Amz-Meta-My-Key: 2          <- expected: x-amz-meta-my-key: 2
```

AWS always emits lower-case `x-amz-meta-*`. botocore keeps whatever case the
server sent, so `head_object()['Metadata']` is `{'My-Key': '2'}` instead of
`{'my-key': '2'}`; code that looks up `Metadata['my-key']` (or compares
metadata dicts after a copy) breaks. Same on HEAD, GET and after CopyObject
with `x-amz-metadata-directive: REPLACE`. s3-tests: 11 failures
(`KeyError: 'meta1'`, `{'Foo': 'bar'} != {'foo': 'bar'}`). In Go this needs
`w.Header()["x-amz-meta-"+k] = []string{v}` (direct map assignment) rather than
`Header().Set`, which canonicalises.

---

## B. Wrong status code / wrong behaviour

### B1. CreateBucket on an existing bucket

- Own bucket, region us-east-1: `PUT /b` (second time) -> `409
  BucketAlreadyOwnedByYou`. Expected: `200 OK` (AWS legacy behaviour in
  us-east-1, which is the region this server reports; idempotent
  "ensure bucket" code relies on it). s3-tests: `test_bucket_create_exists`,
  `test_bucket_recreate_not_overriding`, `test_bucket_list_return_data_versioning`,
  `test_versioning_concurrent_multi_object_delete`.
- Bucket owned by *another* user: `PUT /b` as user `alt` -> `409
  BucketAlreadyOwnedByYou` with message `bucket "b" already exists`. Expected
  code: `BucketAlreadyExists`. s3-tests: `test_bucket_recreate_overwrite_acl`,
  `test_bucket_recreate_new_acl`.

### B2. CompleteMultipartUpload is not idempotent

```
POST /b/k?uploadId=U  (parts list)   -> 200
POST /b/k?uploadId=U  (same body)    -> 404 NoSuchUpload
```

Expected: 200 with the same result. SDKs retry CompleteMultipartUpload when
the first response is lost; with this behaviour the retry reports failure for
an upload that actually succeeded. s3-tests: `test_multipart_upload`,
`test_multipart_upload_small`.

### B3. `If-Match` on a missing key returns 412 instead of 404

```
PUT /b/missing     If-Match: "deadbeef"   -> 412 PreconditionFailed
```

Expected: `404 NoSuchKey` (AWS conditional-write docs). `If-Match` with a wrong
ETag on an existing key correctly returns 412, and `If-None-Match: *` works.
s3-tests: `test_put_current_object_if_match`. Also, `If-None-Match: "<etag>"`
(specific ETag, not `*`) on PUT is ignored (200); s3-tests
`test_put_object_if_match` expects 412. AWS documents only `*` here, so this
one is low priority.

### B4. Streaming uploads: `aws-chunked` Content-Encoding is stored

An aws-chunked upload with trailing checksum is decoded correctly, but the
transport encoding leaks into the object:

```
PUT /b/trailer
x-amz-content-sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER
x-amz-decoded-content-length: 100000
x-amz-trailer: x-amz-checksum-crc32
Content-Encoding: aws-chunked            (or: aws-chunked, gzip)
                                         -> 200, X-Amz-Checksum-Crc32 correct
HEAD /b/trailer                          -> Content-Encoding: aws-chunked   (or: aws-chunked, gzip)
```

Expected: `aws-chunked` stripped (no header, or `gzip`). AWS SDK for Java v2,
JS v3 and Go v2 use this mode for streaming/checksummed uploads over TLS; a
browser later fetching such an object through a presigned URL fails to decode
the body. s3-tests: `test_object_content_encoding_aws_chunked`. (rclone,
restic, s3cmd and aws cli did not hit this in the runs here.)

### B5. `Transfer-Encoding: chunked` without Content-Length is rejected

```
PUT /b/chunked   Transfer-Encoding: chunked   (SigV4 presigned, UNSIGNED-PAYLOAD)
HTTP/1.1 411 Length Required   (MissingContentLength)
```

Expected: 200 (AWS accepts plain HTTP chunked bodies for unsigned payloads).
s3-tests: `test_object_write_with_chunked_transfer_encoding`.

### B6. Anonymous requests to a nonexistent bucket get 403

```
GET /nosuchbucket/o.txt   (no auth)   -> 403 AccessDenied
DELETE /deletedbucket/k   (no auth)   -> 403 AccessDenied
```

Expected: `404 NoSuchBucket` (AWS reports bucket non-existence before
authorisation). Authenticated requests correctly get 404. s3-tests:
`test_object_raw_get_bucket_gone`, `test_object_delete_key_bucket_gone`.

### B7. Bucket policy parser rejects valid policies

- Any statement with a `Condition` block -> `400 MalformedPolicy: statement 0:
  Condition is not supported` (e.g. `aws:SourceIp`, `s3:prefix`,
  `s3:x-amz-acl`). s3-tests: 11 failures.
- `"Action": "*"` -> `400 MalformedPolicy: action "*" must start with s3:`.
  AWS accepts `*` (and `s3:*`). s3-tests:
  `test_multipart_upload_on_a_bucket_with_policy`.

### B8. Multipart edge cases

- `GET /b/k?partNumber=2` on a single-part object -> `416 InvalidRange`.
  Expected `400 InvalidPartNumber` (s3-tests `test_multipart_get_part`,
  `test_multipart_single_get_part`).
- `UploadPartCopy` with `x-amz-copy-source-range: bytes=0-20` on a 10-byte
  source -> 200. Expected `400 InvalidArgument` (`bytes=5-2` is correctly
  rejected). s3-tests `test_multipart_copy_invalid_range`.
- `CompleteMultipartUpload` whose part list names PartNumber 1 twice, where
  the first entry's ETag belongs to an upload of that part that was later
  overwritten -> `400 InvalidPart`. AWS resolves it. s3-tests
  `test_multipart_resend_first_finishes_last`. (Plain re-upload of a part and
  completing with the newest ETag works.)
- `CompleteMultipartUpload` with no request body -> `MissingRequestBodyError`;
  AWS answers `MalformedXML`. s3-tests `test_multipart_upload_empty`.

### B9. Listing details

- `GET /b?max-keys=0` (v1 and v2) -> `IsTruncated=true`. Expected `false`
  (and no NextMarker/NextContinuationToken). A naive "loop while IsTruncated"
  with max-keys=0 never terminates.
- v1 `GET /b` never includes `<Marker>`; AWS always emits `<Marker></Marker>`
  (botocore `KeyError: 'Marker'`). v2 `GET /b?list-type=2&continuation-token=`
  does not echo `<ContinuationToken>`.
- v1 with `encoding-type=url`: `<Prefix>` is URL-encoded (`%0A` for a newline
  prefix). botocore decodes `Delimiter/Marker/NextMarker/Key` for v1 but not
  `Prefix`, so the test sees `%0A`; v2 is fine. Low priority / possibly an
  s3-tests-vs-botocore quirk.

---

## C. Cosmetic / minor

- Tag sets come back in a *random* order that changes from request to
  request (`PUT ?tagging` `[1=0, 0=1]` -> `GET ?tagging` `[0=1, 1=0]` one time,
  `[1=0, 0=1]` the next; looks like Go map iteration). AWS preserves the
  order given. s3-tests: `test_get_obj_tagging`, `test_put_max_tags`,
  `test_put_modify_tags`, `test_put_delete_tags`, `test_set_multipart_tagging`
  fail intermittently; the three that sometimes pass are kept out of
  `conformance/allowlist.txt` for that reason.
- `GetObjectAttributes` returns an (empty) `ObjectParts` block for single-part
  objects and omits `PartsCount` for multipart ones.
- Lifecycle: a rule without `ID` is stored without one (AWS generates an ID);
  `ID` longer than 255 chars and `Expiration.Date` not at midnight UTC are
  accepted (AWS: 400). s3-tests `test_lifecycle_get_no_id`,
  `test_lifecycle_id_too_long`, `test_lifecycle_set_invalid_date`.
- Presigned URL with `X-Amz-Expires=0` or `> 604800` -> `400
  AuthorizationHeaderMalformed`; AWS uses `AuthorizationQueryParametersError`
  (s3-tests expects 403 here, which is an RGW convention).
- `HEAD` of a nonexistent bucket returns a bare `404` with no XML body
  (fine for HEAD; noted because botocore reports the code as `"404"`).
- `GetBucketPolicyStatus` is not implemented (returns `NoSuchBucketPolicy`).
- Anonymous `GET /b/missing.txt` on a public bucket correctly returns 404
  NoSuchKey (good; noted for completeness).

---

## D. Unsupported features and design differences (informational)

Not bugs as such, but they are what the remaining s3-tests failures are:

- **POST Object (HTML form upload)** -> `501 NotImplemented` ("A header that
  you provided implies functionality that is not implemented"). 35 s3-tests
  failures (`test_post_object_*`); browser direct-upload flows do not work.
- **Authorisation model**: a user with role `read-write` (bucket scope `*`)
  can list, read, write, copy into and delete objects in buckets created by
  another user; there is no owner-based isolation and bucket policies only
  add permissions (tests that expect a second user to be *denied* without an
  explicit policy fail: 27 `ClientError not raised`). Document this clearly;
  anyone porting an AWS multi-user setup will be surprised.
- Not implemented: bucket ownership controls, public access block, object
  lock default retention / governance bypass, GetObjectTorrent, SigV2,
  tenants (`tenant:bucket` names). Object lock exists partially
  (`PutObjectLockConfiguration` -> `NotImplemented: Default retention rules`).
- `GetBucketLocation` returns an empty `LocationConstraint` (us-east-1
  semantics) and `CreateBucketConfiguration/LocationConstraint` is ignored
  on create.
- `x-amz-version-id` is (correctly) not returned on unversioned buckets.

---

## E. Tooling notes (admin CLI / quickstart)

- (Resolved 2026-10-12.) Earlier runs hit a fixed Raft port in `quickstart`
  that made concurrent quickstarts race. Clustering has since been removed
  (ADR 0005); `quickstart` only binds the S3 listen address and the harness
  runners no longer retry.

---

## Reproduction helpers

- `test/clients/run.sh` (A1, A4, A5, A2 via `boto3_smoke.py`)
- `test/conformance/run.sh --discover` (everything else; failing node ids and
  first-line messages are written to `failing.txt` in `OUT_DIR`)
- A1 raw headers: `curl -s -D - -o /dev/null -H 'Range: bytes=0-99' -H
  'x-amz-checksum-mode: ENABLED' "$(aws --endpoint-url $URL s3 presign s3://b/k)"`
