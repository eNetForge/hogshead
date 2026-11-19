"""pytest plugin loaded by run.sh (-p hogshead_s3tests_plugin).

Works around one hogshead incompatibility so that it does not mask every
other result: the server refuses DeleteBucket while multipart uploads are in
progress (AWS only does that for directory buckets), and the s3-tests bucket
sweep never aborts uploads, so a single test that leaves an upload behind
makes every later test in the same worker error at setup.

The sweep (s3tests.functional.nuke_bucket) is wrapped to abort in-progress
uploads first.  Set S3TESTS_ABORT_MPU_IN_NUKE=0 to disable the workaround and
observe the raw behaviour.  The incompatibility itself is recorded in
test/REPORT.md.
"""
import os


def _abort_uploads(client, bucket):
    kwargs = {"Bucket": bucket}
    while True:
        resp = client.list_multipart_uploads(**kwargs)
        for up in resp.get("Uploads", []):
            try:
                client.abort_multipart_upload(Bucket=bucket, Key=up["Key"], UploadId=up["UploadId"])
            except Exception:  # noqa: BLE001 - best effort cleanup
                pass
        if not resp.get("IsTruncated"):
            return
        kwargs["KeyMarker"] = resp.get("NextKeyMarker")
        kwargs["UploadIdMarker"] = resp.get("NextUploadIdMarker")


def pytest_configure(config):
    if os.environ.get("S3TESTS_ABORT_MPU_IN_NUKE", "1") == "0":
        return
    import s3tests.functional as functional

    original = functional.nuke_bucket

    def nuke_bucket(client, bucket):
        try:
            _abort_uploads(client, bucket)
        except Exception:  # noqa: BLE001 - the original sweep reports real errors
            pass
        return original(client, bucket)

    functional.nuke_bucket = nuke_bucket
