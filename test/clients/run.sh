#!/usr/bin/env bash
# External-client compatibility suite for hogshead.
#
# Starts a throwaway server, then runs one suite per client (aws cli, s3cmd,
# rclone, restic, boto3) and prints PASS/FAIL/SKIP per suite.  Exits non-zero
# if any suite failed.  A client that cannot be found is SKIPped, not failed.
#
# Environment (all optional):
#   PORT          S3 port                                  (default 19100)
#   HOGSHEAD_BIN    server binary                            (default ./bin/hogshead)
#   SCRATCH       scratch root for server data + work dirs (default mktemp -d)
#   CLIENTS_VENV  venv (root or bin dir) holding aws, s3cmd and python+boto3
#   AWS_BIN       aws cli binary (overrides PATH/CLIENTS_VENV lookup)
#   S3CMD_BIN     s3cmd binary (overrides lookup)
#   RCLONE_BIN    rclone binary (overrides lookup)
#   RESTIC_BIN    restic binary (overrides lookup)
#   PYTHON_BIN    python with boto3 (overrides lookup)
#   STEP_TIMEOUT  seconds per step before it is killed and failed (default 600)
#   ONLY          space-separated subset of suites to run, e.g. "aws rclone"
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
PORT=${PORT:-19100}
HOGSHEAD_BIN=${HOGSHEAD_BIN:-./bin/hogshead}
[ -x "$HOGSHEAD_BIN" ] || HOGSHEAD_BIN=$ROOT/bin/hogshead
SCRATCH=${SCRATCH:-$(mktemp -d)}
STEP_TIMEOUT=${STEP_TIMEOUT:-600}
ONLY=${ONLY:-"aws s3cmd rclone restic boto3 velero"}
ENDPOINT="http://127.0.0.1:$PORT"
WORK="$SCRATCH/clients-$PORT"
LOGS="$WORK/logs"

log() { printf '[clients] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# ---------------------------------------------------------------- tool lookup
find_tool() { # name [override] -> path or empty
  local name=$1 override=${2:-}
  if [ -n "$override" ]; then [ -x "$override" ] && echo "$override"; return 0; fi
  if [ -n "${CLIENTS_VENV:-}" ]; then
    for c in "$CLIENTS_VENV/bin/$name" "$CLIENTS_VENV/$name"; do [ -x "$c" ] && { echo "$c"; return 0; }; done
  fi
  command -v "$name" 2>/dev/null || true
}
AWS=$(find_tool aws "${AWS_BIN:-}")
S3CMD=$(find_tool s3cmd "${S3CMD_BIN:-}")
RCLONE=$(find_tool rclone "${RCLONE_BIN:-}")
RESTIC=$(find_tool restic "${RESTIC_BIN:-}")
PYTHON=$(find_tool python "${PYTHON_BIN:-}")
[ -n "$PYTHON" ] && "$PYTHON" -c 'import boto3' 2>/dev/null || PYTHON=$(command -v python3 2>/dev/null || true)
[ -n "$PYTHON" ] && "$PYTHON" -c 'import boto3' 2>/dev/null || PYTHON=

[ -x "$HOGSHEAD_BIN" ] || die "server binary not found at $HOGSHEAD_BIN (run: go build -o bin/hogshead ./cmd/hogshead)"
rm -rf "$WORK"; mkdir -p "$LOGS"

# ---------------------------------------------------------------- server
export HOGSHEAD_ADMIN_SOCKET=
QS=$("$HOGSHEAD_BIN" quickstart --data-dir "$WORK/data" --config "$WORK/config.toml" --listen "127.0.0.1:$PORT" 2>"$LOGS/quickstart.log") || die "quickstart failed: $(tail -n 3 "$LOGS/quickstart.log")"
export AWS_ACCESS_KEY_ID; AWS_ACCESS_KEY_ID=$(printf '%s\n' "$QS" | sed -n 's/^ *AWS_ACCESS_KEY_ID=//p')
export AWS_SECRET_ACCESS_KEY; AWS_SECRET_ACCESS_KEY=$(printf '%s\n' "$QS" | sed -n 's/^ *AWS_SECRET_ACCESS_KEY=//p')
[ -n "$AWS_ACCESS_KEY_ID" ] && [ -n "$AWS_SECRET_ACCESS_KEY" ] || die "could not parse credentials from quickstart output"
"$HOGSHEAD_BIN" serve --config "$WORK/config.toml" >"$LOGS/server.log" 2>&1 &
SERVER_PID=$!
cleanup() {
  if kill -0 "$SERVER_PID" 2>/dev/null; then kill "$SERVER_PID" 2>/dev/null || true; wait "$SERVER_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT
for _ in $(seq 1 60); do
  [ "$(curl -s "$ENDPOINT/readyz" 2>/dev/null || true)" = ready ] && break
  kill -0 "$SERVER_PID" 2>/dev/null || die "server exited early; see $LOGS/server.log"
  sleep 0.5
done
[ "$(curl -s "$ENDPOINT/readyz" 2>/dev/null || true)" = ready ] || die "server never became ready; see $LOGS/server.log"
log "server ready at $ENDPOINT (work dir $WORK)"

export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true AWS_PAGER= AWS_SHARED_CREDENTIALS_FILE=/dev/null
export AWS_CONFIG_FILE="$WORK/aws-config"
printf '[default]\nregion = us-east-1\ns3 =\n    addressing_style = path\ncli_pager =\n' >"$AWS_CONFIG_FILE"

# ---------------------------------------------------------------- step runner
CUR_SUITE=; STEP_N=0; SUITE_FAILED=0
declare -A RESULT
step() { # description command...  (runs the command, records ok/FAIL, continues)
  local desc=$1; shift
  STEP_N=$((STEP_N + 1))
  local logf="$LOGS/$CUR_SUITE.$(printf '%02d' "$STEP_N").log"
  { printf '$ %q' "$1"; printf ' %q' "${@:2}"; printf '\n'; } >"$logf"
  if timeout "$STEP_TIMEOUT" "$@" >>"$logf" 2>&1; then
    printf '    ok    %s\n' "$desc"
  else
    local rc=$?
    SUITE_FAILED=1
    printf '    FAIL  %s (exit %d)\n' "$desc" "$rc"
    tail -n 4 "$logf" | sed 's/^/          | /'
  fi
}
begin_suite() { CUR_SUITE=$1; STEP_N=0; SUITE_FAILED=0; printf '==> %s\n' "$1"; }
end_suite() { if [ "$SUITE_FAILED" = 0 ]; then RESULT[$CUR_SUITE]=PASS; else RESULT[$CUR_SUITE]=FAIL; fi; printf '<== %s: %s\n' "$CUR_SUITE" "${RESULT[$CUR_SUITE]}"; }
skip_suite() { RESULT[$1]=SKIP; printf '==> %s\n    SKIP  %s\n<== %s: SKIP\n' "$1" "$2" "$1"; }
wants() { case " $ONLY " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

make_fixtures() { # dir -> small, 12 MiB, tree with spaces/unicode
  local d=$1; mkdir -p "$d/tree/sub dir"
  printf 'hello hogshead\n' >"$d/small.txt"
  head -c 12582912 /dev/urandom >"$d/big.bin"
  cp "$d/big.bin" "$d/tree/big.bin"
  printf 'spaces\n' >"$d/tree/sub dir/file with spaces.txt"
  printf 'unicode\n' >"$d/tree/sub dir/ünïcödé ✓.txt"
  printf 'plus\n' >"$d/tree/plus+sign.txt"
  printf 'a\n' >"$d/tree/a.txt"
}
contains() { grep -q -- "$2" "$1"; }

# ---------------------------------------------------------------- aws cli
aws_purge_versions() { # bucket: delete every version and delete marker
  local b=$1 json
  json=$("$AWS" --endpoint-url "$ENDPOINT" s3api list-object-versions --bucket "$b" --output json \
    --query '{Objects: [Versions[].{Key:Key,VersionId:VersionId}, DeleteMarkers[].{Key:Key,VersionId:VersionId}][]}')
  if [ "$(printf '%s' "$json" | tr -d '[:space:]')" != '{"Objects":null}' ] && [ "$(printf '%s' "$json" | tr -d '[:space:]')" != '{"Objects":[]}' ]; then
    "$AWS" --endpoint-url "$ENDPOINT" s3api delete-objects --bucket "$b" --delete "$json"
  fi
}
suite_aws() {
  begin_suite aws
  local A=("$AWS" --endpoint-url "$ENDPOINT") W="$WORK/aws" B="aws-cli-$$" BV="aws-cli-ver-$$"
  local NP=--no-progress
  mkdir -p "$W/back" "$W/sync"; make_fixtures "$W"
  local ver; ver=$("$AWS" --version 2>&1 | head -1); log "aws: $ver"
  local cks=()
  case "$ver" in aws-cli/2.*) ;; *) cks=(--checksum-algorithm CRC64NVME) ;; esac  # v2 defaults to CRC64NVME
  step "s3 mb"                       "${A[@]}" s3 mb "s3://$B"
  step "s3 cp small file"            "${A[@]}" s3 cp $NP "$W/small.txt" "s3://$B/small.txt"
  step "s3 cp 12 MiB (multipart, CRC64NVME)" "${A[@]}" s3 cp $NP "${cks[@]}" "$W/big.bin" "s3://$B/big.bin"
  step "s3 ls shows both"            bash -c "'${A[0]}' --endpoint-url '$ENDPOINT' s3 ls 's3://$B/' | tee /dev/stderr | grep -q big.bin"
  step "s3 cp small back + cmp"      bash -c "'${A[0]}' --endpoint-url '$ENDPOINT' s3 cp --no-progress 's3://$B/small.txt' '$W/back/small.txt' && cmp '$W/small.txt' '$W/back/small.txt'"
  step "s3 cp 12 MiB back + cmp"     bash -c "'${A[0]}' --endpoint-url '$ENDPOINT' s3 cp --no-progress 's3://$B/big.bin' '$W/back/big.bin' && cmp '$W/big.bin' '$W/back/big.bin'"
  step "s3 sync directory up"        "${A[@]}" s3 sync $NP "$W/tree" "s3://$B/tree/"
  step "s3 sync directory down + diff" bash -c "'${A[0]}' --endpoint-url '$ENDPOINT' s3 sync --no-progress 's3://$B/tree/' '$W/sync/' && diff -r '$W/tree' '$W/sync'"
  step "s3api head-object"           bash -c "'${A[0]}' --endpoint-url '$ENDPOINT' s3api head-object --bucket '$B' --key big.bin | tee /dev/stderr | grep -q '\"ContentLength\": 12582912'"
  step "s3api get-bucket-location"   "${A[@]}" s3api get-bucket-location --bucket "$B"
  step "s3 presign + curl"           bash -c "u=\$('${A[0]}' --endpoint-url '$ENDPOINT' s3 presign 's3://$B/small.txt' --expires-in 120) && curl -sf \"\$u\" -o '$W/back/presigned.txt' && cmp '$W/small.txt' '$W/back/presigned.txt'"
  step "s3api copy-object"           "${A[@]}" s3api copy-object --bucket "$B" --key small-copy.txt --copy-source "$B/small.txt"
  step "s3api delete-objects"        bash -c "'${A[0]}' --endpoint-url '$ENDPOINT' s3api delete-objects --bucket '$B' --delete 'Objects=[{Key=small-copy.txt},{Key=big.bin}]' | tee /dev/stderr | grep -q small-copy.txt"
  step "s3 rb --force"               "${A[@]}" s3 rb "s3://$B" --force
  step "versioning: mb"              "${A[@]}" s3 mb "s3://$BV"
  step "s3api put-bucket-versioning" "${A[@]}" s3api put-bucket-versioning --bucket "$BV" --versioning-configuration Status=Enabled
  step "s3api get-bucket-versioning" bash -c "'${A[0]}' --endpoint-url '$ENDPOINT' s3api get-bucket-versioning --bucket '$BV' | grep -q Enabled"
  step "put two versions"            bash -c "'${A[0]}' --endpoint-url '$ENDPOINT' s3 cp --no-progress '$W/small.txt' 's3://$BV/v.txt' && '${A[0]}' --endpoint-url '$ENDPOINT' s3 cp --no-progress '$W/tree/a.txt' 's3://$BV/v.txt'"
  step "s3api list-object-versions (2 versions)" bash -c "'${A[0]}' --endpoint-url '$ENDPOINT' s3api list-object-versions --bucket '$BV' --query 'length(Versions)' --output text | tee /dev/stderr | grep -qx 2"
  step "s3 rm creates delete marker" bash -c "'${A[0]}' --endpoint-url '$ENDPOINT' s3 rm 's3://$BV/v.txt' && '${A[0]}' --endpoint-url '$ENDPOINT' s3api list-object-versions --bucket '$BV' --query 'length(DeleteMarkers)' --output text | grep -qx 1"
  step "purge versions + rb"         bash -c "$(declare -f aws_purge_versions); AWS='$AWS'; ENDPOINT='$ENDPOINT'; aws_purge_versions '$BV' && '$AWS' --endpoint-url '$ENDPOINT' s3 rb 's3://$BV'"
  end_suite
}

# ---------------------------------------------------------------- s3cmd
suite_s3cmd() {
  begin_suite s3cmd
  local W="$WORK/s3cmd" B="s3cmd-$$" C
  mkdir -p "$W/back"; make_fixtures "$W"
  cat >"$W/s3cfg" <<CFG
[default]
access_key = $AWS_ACCESS_KEY_ID
secret_key = $AWS_SECRET_ACCESS_KEY
host_base = 127.0.0.1:$PORT
host_bucket = 127.0.0.1:$PORT/%(bucket)
use_https = False
signature_v2 = False
bucket_location = us-east-1
CFG
  C=("$S3CMD" -c "$W/s3cfg")
  printf '<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod><AllowedHeader>*</AllowedHeader></CORSRule></CORSConfiguration>\n' >"$W/cors.xml"
  step "mb"                          "${C[@]}" mb "s3://$B"
  step "put small"                   "${C[@]}" put "$W/small.txt" "s3://$B/small.txt"
  step "put 12 MiB (multipart 5 MiB chunks)" "${C[@]}" put --multipart-chunk-size-mb=5 "$W/big.bin" "s3://$B/big.bin"
  step "ls"                          bash -c "'${C[0]}' -c '$W/s3cfg' ls 's3://$B/' | tee /dev/stderr | grep -q big.bin"
  step "get small + cmp"             bash -c "'${C[0]}' -c '$W/s3cfg' get 's3://$B/small.txt' '$W/back/small.txt' && cmp '$W/small.txt' '$W/back/small.txt'"
  step "get 12 MiB + cmp"            bash -c "'${C[0]}' -c '$W/s3cfg' get 's3://$B/big.bin' '$W/back/big.bin' && cmp '$W/big.bin' '$W/back/big.bin'"
  step "info object"                 "${C[@]}" info "s3://$B/small.txt"
  step "info bucket"                 "${C[@]}" info "s3://$B"
  step "setacl --acl-public (object)" "${C[@]}" setacl --acl-public "s3://$B/small.txt"
  step "anonymous curl of public object" bash -c "code=\$(curl -s -o /dev/null -w '%{http_code}' '$ENDPOINT/$B/small.txt'); echo \"HTTP \$code\"; [ \"\$code\" = 200 ]"
  step "setacl --acl-public (bucket)" "${C[@]}" setacl --acl-public "s3://$B"
  step "anonymous curl after bucket public" bash -c "code=\$(curl -s -o /dev/null -w '%{http_code}' '$ENDPOINT/$B/big.bin'); echo \"HTTP \$code\"; [ \"\$code\" = 200 ]"
  step "setacl --acl-private (bucket)" "${C[@]}" setacl --acl-private "s3://$B"
  if "$S3CMD" --help 2>&1 | grep -q setcors; then
    step "setcors"                   "${C[@]}" setcors "$W/cors.xml" "s3://$B"
    step "delcors"                   "${C[@]}" delcors "s3://$B"
  else
    printf '    skip  setcors (not supported by this s3cmd)\n'
  fi
  step "del"                         "${C[@]}" del "s3://$B/small.txt" "s3://$B/big.bin"
  step "rb"                          "${C[@]}" rb "s3://$B"
  end_suite
}

# ---------------------------------------------------------------- rclone
suite_rclone() {
  begin_suite rclone
  local W="$WORK/rclone" B="rclone-$$"
  mkdir -p "$W/back"; make_fixtures "$W"
  export RCLONE_CONFIG_T_TYPE=s3 RCLONE_CONFIG_T_PROVIDER=Other RCLONE_CONFIG_T_ENDPOINT="$ENDPOINT" RCLONE_CONFIG_T_REGION=us-east-1 \
         RCLONE_CONFIG_T_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" RCLONE_CONFIG_T_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY"
  local R=("$RCLONE" --config /dev/null)
  step "mkdir"                       "${R[@]}" mkdir "T:$B"
  step "copy tree (5M cutoff/chunks, unicode names)" "${R[@]}" copy --s3-upload-cutoff 5M --s3-chunk-size 5M "$W/tree" "T:$B"
  step "ls"                          "${R[@]}" ls "T:$B"
  step "check"                       "${R[@]}" check "$W/tree" "T:$B"
  step "copy back + diff"            bash -c "'$RCLONE' --config /dev/null copy 'T:$B' '$W/back' && diff -r '$W/tree' '$W/back' && cmp '$W/tree/big.bin' '$W/back/big.bin'"
  step "delete"                      "${R[@]}" delete "T:$B"
  step "purge"                       "${R[@]}" purge "T:$B"
  step "bucket gone"                 bash -c "! '$RCLONE' --config /dev/null lsd T: | grep -q ' $B\$'"
  end_suite
}

# ---------------------------------------------------------------- restic
suite_restic() {
  begin_suite restic
  local W="$WORK/restic" B="restic-$$"
  mkdir -p "$W/src/sub" "$W/restore" "$W/cache"
  head -c 3000000 /dev/urandom >"$W/src/a.bin"; head -c 20000000 /dev/urandom >"$W/src/sub/b.bin"; printf 'c\n' >"$W/src/c.txt"
  export RESTIC_PASSWORD=hogshead-test RESTIC_REPOSITORY="s3:$ENDPOINT/$B" RESTIC_CACHE_DIR="$W/cache"
  local R=("$RESTIC" --no-lock=false)
  R=("$RESTIC")
  step "init"                        "${R[@]}" init
  step "backup"                      "${R[@]}" backup "$W/src"
  step "backup (second snapshot)"    bash -c "printf 'd\n' >'$W/src/d.txt' && '$RESTIC' backup '$W/src'"
  step "check"                       "${R[@]}" check
  step "restore latest + diff"       bash -c "'$RESTIC' restore latest --target '$W/restore' && diff -r '$W/src' '$W/restore$W/src'"
  step "forget --prune --keep-last 1" "${R[@]}" forget --prune --keep-last 1
  step "check after prune"           "${R[@]}" check
  step "one snapshot left"           bash -c "'$RESTIC' snapshots --json | '${PYTHON:-python3}' -c 'import json,sys; s=json.load(sys.stdin); print(len(s), \"snapshots\"); sys.exit(0 if len(s)==1 else 1)'"
  end_suite
}

# ---------------------------------------------------------------- boto3
suite_boto3() {
  begin_suite boto3
  step "boto3_smoke.py"              env S3_ENDPOINT="$ENDPOINT" "$PYTHON" "$HERE/boto3_smoke.py"
  if [ -f "$LOGS/boto3.01.log" ]; then grep -E '^(ok|FAIL|SKIP) ' "$LOGS/boto3.01.log" | sed 's/^/          /'; fi
  end_suite
}

# ---------------------------------------------------------------- run
if wants aws;    then if [ -n "$AWS" ];    then suite_aws;    else skip_suite aws "aws cli not found (set CLIENTS_VENV or AWS_BIN)"; fi; fi
if wants s3cmd;  then if [ -n "$S3CMD" ];  then suite_s3cmd;  else skip_suite s3cmd "s3cmd not found (set CLIENTS_VENV or S3CMD_BIN)"; fi; fi
if wants rclone; then if [ -n "$RCLONE" ]; then suite_rclone; else skip_suite rclone "rclone not found (set RCLONE_BIN)"; fi; fi
if wants restic; then if [ -n "$RESTIC" ]; then suite_restic; else skip_suite restic "restic not found (set RESTIC_BIN)"; fi; fi
if wants boto3;  then if [ -n "$PYTHON" ]; then suite_boto3;  else skip_suite boto3 "python with boto3 not found (set CLIENTS_VENV or PYTHON_BIN)"; fi; fi
if wants velero; then skip_suite velero "not installed; see test/clients/README.md for the manual procedure"; fi

printf '\n%-8s %s\n' SUITE RESULT
failed=0
for s in aws s3cmd rclone restic boto3 velero; do
  [ -n "${RESULT[$s]:-}" ] || continue
  printf '%-8s %s\n' "$s" "${RESULT[$s]}"
  [ "${RESULT[$s]}" = FAIL ] && failed=1
done
printf 'logs: %s\n' "$LOGS"
exit $failed
