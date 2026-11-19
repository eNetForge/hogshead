# External client compatibility suite

`run.sh` starts a throwaway hogshead server and drives it with real clients:
aws cli, s3cmd, rclone, restic and boto3 (`boto3_smoke.py`). Each suite prints
one line per step and a PASS/FAIL/SKIP verdict; the script exits non-zero if
any suite failed. A client that is not installed makes its suite SKIP.

```sh
go build -o bin/hogshead ./cmd/hogshead

# tools on PATH
test/clients/run.sh

# tools in a venv / downloaded binaries
python3 -m venv /path/venv && /path/venv/bin/pip install awscli s3cmd 'boto3' 'botocore[crt]'
CLIENTS_VENV=/path/venv RESTIC_BIN=/path/restic AWS_BIN=/path/aws-v2 test/clients/run.sh

ONLY="rclone boto3" PORT=19150 test/clients/run.sh     # subset, other port
```

Environment knobs are documented at the top of `run.sh`. Logs for every step
are written under `$SCRATCH/clients-$PORT/logs/`.

Notes:

- aws cli **v2** is preferred (`AWS_BIN`): it uses CRC64NVME full-object
  checksums and ranged parallel downloads by default, which is exactly the
  code path that matters for compatibility. With the pip-installed v1 cli the
  suite passes `--checksum-algorithm CRC64NVME` explicitly.
- `botocore[crt]` (awscrt) is needed for the CRC32C/CRC64NVME checks in
  `boto3_smoke.py`; without it those checks report SKIP.
- The server's Raft port is derived from `PORT` (+300) so two suites can run
  side by side.

## Velero (not automated)

Velero is not run by this suite because it needs a Kubernetes cluster. To
exercise it manually against hogshead:

1. Start a server reachable from the cluster (`hogshead serve --config ...`),
   create a bucket (`aws --endpoint-url $URL s3 mb s3://velero`) and a
   dedicated read-write user/key
   (`hogshead user add velero --role read-write --buckets velero`,
   `hogshead key create velero --json`).
2. Write the credentials file:
   ```
   [default]
   aws_access_key_id = <access key>
   aws_secret_access_key = <secret key>
   ```
3. Install Velero with the AWS plugin against the custom endpoint:
   ```sh
   velero install \
     --provider aws \
     --plugins velero/velero-plugin-for-aws:v1.11.0 \
     --bucket velero \
     --secret-file ./credentials-velero \
     --use-volume-snapshots=false \
     --backup-location-config region=us-east-1,s3ForcePathStyle="true",s3Url=$URL
   ```
4. Verify: `velero backup-location get` must show `Available`; then
   `velero backup create test --include-namespaces default`,
   `velero backup describe test --details`, `velero backup download test`,
   `velero restore create --from-backup test`, and finally
   `velero backup delete test` (which exercises DeleteObjects on the
   `backups/test/` prefix).

Velero's plugin uses the AWS SDK for Go v2 with path-style addressing, SigV4,
multipart uploads for backup tarballs and ListObjectsV2 with prefixes and
delimiters; all of these are covered indirectly by the rclone and boto3 suites.
