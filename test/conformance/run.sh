#!/usr/bin/env bash
# ceph/s3-tests conformance runner for hogshead.
#
#   test/conformance/run.sh                 run exactly allowlist.txt; fail if any fails
#   test/conformance/run.sh --discover      run the broad subset, report newly passing tests
#   test/conformance/run.sh --discover --write-allowlist
#                                           ...and overwrite allowlist.txt with the passing set
#   test/conformance/run.sh -- <pytest args> run with custom pytest arguments (server + config only)
#
# Environment (all optional):
#   PORT            S3 port for the throwaway server            (default 19200)
#   HOGSHEAD_BIN      server binary                               (default ./bin/hogshead)
#   SCRATCH         where server data, venv and clone live      (default mktemp -d)
#   S3TESTS_DIR     existing clone of ceph/s3-tests             (default $SCRATCH/s3-tests)
#   S3TESTS_VENV    existing venv with its requirements         (default $SCRATCH/s3tests-venv)
#   S3TESTS_REF     git ref to clone                            (default master)
#   TEST_TIMEOUT    per-test timeout in seconds                 (default 30)
#   JOBS            pytest-xdist workers, 0 disables            (default 4)
#   S3TESTS_MARKERS pytest -m expression for --discover         (see MARKERS below)
#   S3TESTS_KEYWORDS pytest -k expression for --discover        (see KEYWORDS below)
#   OUT_DIR         where junit.xml / passing.txt / failing.txt go (default $SCRATCH/conformance-results)
#   S3TESTS_ABORT_MPU_IN_NUKE  set to 0 to disable the bucket-sweep workaround (see hogshead_s3tests_plugin.py)
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
PORT=${PORT:-19200}
HOGSHEAD_BIN=${HOGSHEAD_BIN:-$ROOT/bin/hogshead}
SCRATCH=${SCRATCH:-$(mktemp -d)}
S3TESTS_DIR=${S3TESTS_DIR:-$SCRATCH/s3-tests}
S3TESTS_VENV=${S3TESTS_VENV:-$SCRATCH/s3tests-venv}
S3TESTS_REF=${S3TESTS_REF:-master}
TEST_TIMEOUT=${TEST_TIMEOUT:-30}
JOBS=${JOBS:-4}
OUT_DIR=${OUT_DIR:-$SCRATCH/conformance-results}
ALLOWLIST=$HERE/allowlist.txt
TEMPLATE=$HERE/s3tests.conf.template
ENDPOINT="http://127.0.0.1:$PORT"

# Markers excluded from the broad run: features hogshead does not implement
# (or that need rgw-specific plumbing) and tests that fail on AWS itself.
MARKERS=${S3TESTS_MARKERS:-"not fails_on_aws and not lifecycle_expiration and not lifecycle_transition and not sse_s3 and not encryption and not bucket_encryption and not s3select and not s3website and not cloud_transition and not cloud_restore and not test_of_sts and not iam_tenant and not iam_account and not iam_role and not iam_user and not iam_cross_account and not webidentity_test and not fails_strict_rfc2616 and not bucket_logging and not checksum and not storage_class and not object_lock and not sns and not s3control and not appendobject and not abac_test"}
# Node-id substrings for the broad run (pytest -k): the areas we care about.
KEYWORDS=${S3TESTS_KEYWORDS:-"bucket_list or bucket_listv2 or bucket_create or bucket_delete or bucket_head or bucket_get or bucket_recreate or bucket_notexist or object_ or multipart or versioning or versioned or copy or delete_objects or multi_object_delete or policy or cors or lifecycle_set or lifecycle_get or lifecycle_delete or lifecycle_rules or lifecycle_id or lifecycle_invalid or lifecycle_same_prefix or presigned or acl_canned or tagging or range or ranged or if_match or ifmatch or if_none_match or ifnonematch or conditional or encoding or marker or max_keys or maxkeys or prefix or delimiter or put_get or get_object or head_object or head_bucket or get_bucket or location or atomic or append"}

MODE=allowlist
WRITE_ALLOWLIST=0
EXTRA_PYTEST=()
while [ $# -gt 0 ]; do
  case "$1" in
    --discover) MODE=discover ;;
    --write-allowlist) WRITE_ALLOWLIST=1 ;;
    --) shift; MODE=custom; EXTRA_PYTEST=("$@"); break ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

log() { printf '[conformance] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

[ -x "$HOGSHEAD_BIN" ] || die "server binary not found at $HOGSHEAD_BIN (run: go build -o bin/hogshead ./cmd/hogshead)"
mkdir -p "$SCRATCH" "$OUT_DIR"

# ---------------------------------------------------------------- s3-tests
if [ ! -d "$S3TESTS_DIR/s3tests" ]; then
  log "cloning ceph/s3-tests ($S3TESTS_REF) into $S3TESTS_DIR"
  git clone -q --depth 1 --branch "$S3TESTS_REF" https://github.com/ceph/s3-tests "$S3TESTS_DIR"
fi
PY="$S3TESTS_VENV/bin/python"
if [ ! -x "$PY" ]; then
  log "creating venv at $S3TESTS_VENV"
  python3 -m venv "$S3TESTS_VENV"
  "$PY" -m pip install -q --upgrade pip
  if ! "$PY" -m pip install -q -r "$S3TESTS_DIR/requirements.txt"; then
    log "requirements.txt failed; falling back to tox"
    "$PY" -m pip install -q tox
  fi
  "$PY" -m pip install -q pytest-timeout pytest-xdist || log "pytest-timeout/xdist install failed (continuing)"
fi
"$PY" -c 'import pytest' 2>/dev/null || {
  # requirements failed and tox was installed instead: let tox build its env
  USE_TOX=1
}
USE_TOX=${USE_TOX:-0}
HAVE_TIMEOUT=0; "$PY" -c 'import pytest_timeout' 2>/dev/null && HAVE_TIMEOUT=1
HAVE_XDIST=0; "$PY" -c 'import xdist' 2>/dev/null && HAVE_XDIST=1

# ---------------------------------------------------------------- server
SRV="$SCRATCH/conformance-server-$PORT"
rm -rf "$SRV"; mkdir -p "$SRV"
export HOGSHEAD_ADMIN_SOCKET=
log "quickstart in $SRV (S3 :$PORT)"
QS=$("$HOGSHEAD_BIN" quickstart --data-dir "$SRV/data" --config "$SRV/config.toml" --listen "127.0.0.1:$PORT" 2>"$SRV/quickstart.log") || die "quickstart failed: $(tail -n 3 "$SRV/quickstart.log")"
MAIN_ACCESS_KEY=$(printf '%s\n' "$QS" | sed -n 's/^ *AWS_ACCESS_KEY_ID=//p')
MAIN_SECRET_KEY=$(printf '%s\n' "$QS" | sed -n 's/^ *AWS_SECRET_ACCESS_KEY=//p')
[ -n "$MAIN_ACCESS_KEY" ] && [ -n "$MAIN_SECRET_KEY" ] || die "could not parse admin credentials from quickstart output"

"$HOGSHEAD_BIN" serve --config "$SRV/config.toml" >"$SRV/server.log" 2>&1 &
SERVER_PID=$!
cleanup() {
  if kill -0 "$SERVER_PID" 2>/dev/null; then kill "$SERVER_PID" 2>/dev/null || true; wait "$SERVER_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT
for _ in $(seq 1 60); do
  [ "$(curl -s "$ENDPOINT/readyz" 2>/dev/null || true)" = ready ] && break
  kill -0 "$SERVER_PID" 2>/dev/null || die "server exited early; see $SRV/server.log"
  sleep 0.5
done
[ "$(curl -s "$ENDPOINT/readyz" 2>/dev/null || true)" = ready ] || die "server never became ready; see $SRV/server.log"
log "server ready at $ENDPOINT"

# ---------------------------------------------------------------- users
admin() { local cmd=$1; shift; "$HOGSHEAD_BIN" "$cmd" "$@" --server "$ENDPOINT" --access-key "$MAIN_ACCESS_KEY" --secret-key "$MAIN_SECRET_KEY"; }
make_user() { # name -> prints "access secret"
  admin user add "$1" --role read-write >/dev/null
  admin key create "$1" --json | "$PY" -c 'import json,sys; d=json.load(sys.stdin); print(d["access_key_id"], d["secret_access_key"])'
}
read -r ALT_ACCESS_KEY ALT_SECRET_KEY < <(make_user alt)
read -r TENANT_ACCESS_KEY TENANT_SECRET_KEY < <(make_user tenant)
user_id() { admin user list --json | "$PY" -c 'import json,sys; n=sys.argv[1]; print(next(u["id"] for u in json.load(sys.stdin) if u["name"]==n))' "$1"; }
MAIN_USER_ID=$(user_id admin); ALT_USER_ID=$(user_id alt); TENANT_USER_ID=$(user_id tenant)

# ---------------------------------------------------------------- config
CONF="$SRV/s3tests.conf"
HOST=127.0.0.1 PORT=$PORT MAIN_NAME=admin ALT_NAME=alt TENANT_NAME=tenant \
MAIN_ACCESS_KEY=$MAIN_ACCESS_KEY MAIN_SECRET_KEY=$MAIN_SECRET_KEY MAIN_USER_ID=$MAIN_USER_ID \
ALT_ACCESS_KEY=$ALT_ACCESS_KEY ALT_SECRET_KEY=$ALT_SECRET_KEY ALT_USER_ID=$ALT_USER_ID \
TENANT_ACCESS_KEY=$TENANT_ACCESS_KEY TENANT_SECRET_KEY=$TENANT_SECRET_KEY TENANT_USER_ID=$TENANT_USER_ID \
"$PY" - "$TEMPLATE" "$CONF" <<'PYEOF'
import os, re, sys
src, dst = sys.argv[1:3]
text = open(src).read()
missing = []
def sub(m):
    v = os.environ.get(m.group(1))
    if v is None:
        missing.append(m.group(1)); return m.group(0)
    return v
out = re.sub(r'@([A-Z_]+)@', sub, text)
if missing:
    sys.exit('unset template tokens: ' + ', '.join(missing))
open(dst, 'w').write(out)
PYEOF
log "config rendered to $CONF"

# ---------------------------------------------------------------- pytest
export S3TEST_CONF="$CONF" S3_USE_SIGV4=1
export PYTHONPATH="$HERE${PYTHONPATH:+:$PYTHONPATH}"   # for hogshead_s3tests_plugin
PYTEST_BASE=(-q -p no:cacheprovider -p hogshead_s3tests_plugin -o console_output_style=classic --junitxml="$OUT_DIR/junit.xml")
[ "$HAVE_TIMEOUT" = 1 ] && PYTEST_BASE+=(--timeout "$TEST_TIMEOUT")
[ "$HAVE_XDIST" = 1 ] && [ "$JOBS" -gt 0 ] && PYTEST_BASE+=(-n "$JOBS")

run_pytest() {
  cd "$S3TESTS_DIR"
  if [ "$USE_TOX" = 1 ]; then "$PY" -m tox -- "${PYTEST_BASE[@]}" "$@"
  else "$PY" -m pytest "${PYTEST_BASE[@]}" "$@"
  fi
}

summarize() { # junit.xml -> passing.txt, failing.txt, categories; prints counts
  "$PY" - "$OUT_DIR/junit.xml" "$OUT_DIR" <<'PYEOF'
import collections, re, sys, xml.etree.ElementTree as ET
junit, out = sys.argv[1:3]
root = ET.parse(junit).getroot()
passing, failing, skipped = [], [], []
cats = collections.Counter()
for tc in root.iter('testcase'):
    nid = tc.get('classname', '').replace('.', '/') + '.py::' + tc.get('name')
    fail = tc.find('failure'); err = tc.find('error'); skip = tc.find('skipped')
    if skip is not None:
        skipped.append(nid)
    elif fail is not None or err is not None:
        node = fail if fail is not None else err
        msg = (node.get('message') or '').strip().splitlines()[0] if (node.get('message') or '').strip() else ''
        msg = msg[:200]
        failing.append((nid, msg))
        m = re.search(r'An error occurred \((\w+)\)', msg) or re.search(r'(Timeout)', msg) or re.search(r'(AssertionError|KeyError|TypeError)', msg)
        cats[m.group(1) if m else (msg[:60] or 'unknown')] += 1
    else:
        passing.append(nid)
open(f'{out}/passing.txt', 'w').write(''.join(p + '\n' for p in sorted(passing)))
open(f'{out}/failing.txt', 'w').write(''.join(f'{n}\t{m}\n' for n, m in sorted(failing)))
print(f'passed={len(passing)} failed={len(failing)} skipped={len(skipped)} run={len(passing)+len(failing)}')
if cats:
    print('top failure categories:')
    for c, n in cats.most_common(12):
        print(f'  {n:4d}  {c}')
PYEOF
}

case "$MODE" in
  custom)
    run_pytest "${EXTRA_PYTEST[@]}" || true
    summarize
    ;;
  discover)
    log "discover: running broad subset (markers: $MARKERS)"
    run_pytest -m "$MARKERS" -k "$KEYWORDS" s3tests/functional/test_s3.py || true
    summarize
    LC_ALL=C sort -u "$ALLOWLIST" 2>/dev/null >"$OUT_DIR/allowlist.sorted" || : >"$OUT_DIR/allowlist.sorted"
    LC_ALL=C sort -u "$OUT_DIR/passing.txt" -o "$OUT_DIR/passing.txt"
    LC_ALL=C comm -13 "$OUT_DIR/allowlist.sorted" "$OUT_DIR/passing.txt" >"$OUT_DIR/newly-passing.txt"
    LC_ALL=C comm -12 "$OUT_DIR/allowlist.sorted" <(cut -f1 "$OUT_DIR/failing.txt" | LC_ALL=C sort -u) >"$OUT_DIR/regressions.txt"
    echo "newly passing (not in allowlist): $(wc -l <"$OUT_DIR/newly-passing.txt")"
    sed 's/^/  + /' "$OUT_DIR/newly-passing.txt"
    echo "allowlisted but failing: $(wc -l <"$OUT_DIR/regressions.txt")"
    sed 's/^/  - /' "$OUT_DIR/regressions.txt"
    echo "results in $OUT_DIR (junit.xml, passing.txt, failing.txt)"
    if [ "$WRITE_ALLOWLIST" = 1 ]; then cp "$OUT_DIR/passing.txt" "$ALLOWLIST"; log "allowlist.txt updated ($(wc -l <"$ALLOWLIST") tests)"; fi
    ;;
  allowlist)
    [ -s "$ALLOWLIST" ] || die "allowlist.txt is empty; run with --discover --write-allowlist first"
    mapfile -t IDS < <(grep -v '^\s*#' "$ALLOWLIST" | grep -v '^\s*$')
    log "running ${#IDS[@]} allowlisted tests"
    set +e; run_pytest "${IDS[@]}"; rc=$?; set -e
    summarize
    if [ "$rc" -ne 0 ]; then
      echo "FAIL: allowlisted tests failed:"; cut -f1,2 "$OUT_DIR/failing.txt" | sed 's/^/  - /'
      exit 1
    fi
    echo "PASS: all ${#IDS[@]} allowlisted tests passed"
    ;;
esac
