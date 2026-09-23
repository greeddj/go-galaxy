#!/usr/bin/env bash
# bench.sh - measure ansible-galaxy vs go-galaxy over testing/requirements-*.yml
# in the cold, warm, frozen, s3-* and roles-* scenarios. Knobs, outputs and
# prerequisites: docs/development.md, "The benchmark harness".

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

GG="$ROOT/dist/go-galaxy"
AG="$ROOT/.venv/bin/ansible-galaxy"
OUT="$ROOT/dist/bench"

# Caches and the install target live under $TMPDIR: not in the repository,
# where extracted collections' Go files would reach a tree-walking linter, and
# not in $HOME, so wiping the measured caches never wipes the ones you use.
WORK="${TMPDIR:-/tmp}/go-galaxy-bench"
TARGET="$WORK/target"
GG_CACHE="$WORK/cache/go-galaxy"
AG_CACHE="$WORK/cache/ansible"

RUNS="${RUNS:-5}"
WARMUP="${WARMUP:-1}"
SIZES="${SIZES:-1 10 100}"
SCENARIOS="${SCENARIOS:-cold warm frozen s3-cold s3-warm s3-frozen roles-cold roles-warm}"

S3_ENDPOINT="${S3_ENDPOINT:-http://127.0.0.1:9000}"
S3_BUCKET="${S3_BUCKET:-go-galaxy-bench}"
S3_ACCESS_KEY="${S3_ACCESS_KEY:-local-user}"
S3_SECRET_KEY="${S3_SECRET_KEY:-local-password}"

mkdir -p "$OUT" "$WORK"

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }
}
require hyperfine
require python3
[ -x "$GG" ] || { echo "missing: $GG (run: go build -o ./dist/go-galaxy ./cmd/go-galaxy)" >&2; exit 1; }
[ -x "$AG" ] || { echo "missing: $AG (run: python3 -m venv .venv && .venv/bin/pip install ansible-core)" >&2; exit 1; }

# /usr/bin/time reports peak RSS in bytes on BSD (-l) and in kbytes on GNU
# (-v). Detect once rather than parsing both formats on every measurement.
if /usr/bin/time -l true >/dev/null 2>&1; then
  TIME_FLAVOR="bsd"
else
  TIME_FLAVOR="gnu"
fi

scenario_in_list() {
  case " $SCENARIOS " in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

s3_reachable() {
  curl -fsS -o /dev/null --max-time 5 "$S3_ENDPOINT/minio/health/live" 2>/dev/null
}

# gg_env prints the environment every go-galaxy invocation in this script
# carries, as `k=v` pairs an `env` call can take. The S3 keys are only added
# when a prefix is given, so the same helper drives both backends.
gg_env() {
  local prefix="${1:-}"
  printf '%s ' "GO_GALAXY_CACHE_DIR=$GG_CACHE"
  if [ -n "$prefix" ]; then
    printf '%s ' \
      "GO_GALAXY_S3_ENDPOINT=$S3_ENDPOINT" \
      "GO_GALAXY_S3_BUCKET=$S3_BUCKET" \
      "GO_GALAXY_S3_ACCESS_KEY=$S3_ACCESS_KEY" \
      "GO_GALAXY_S3_SECRET_KEY=$S3_SECRET_KEY" \
      "GO_GALAXY_S3_PREFIX=$prefix"
  fi
}

ag_env() {
  printf '%s ' "ANSIBLE_GALAXY_CACHE_DIR=$AG_CACHE" "ANSIBLE_COLLECTIONS_PATH=$TARGET"
}

# lock_dir prepares a private working directory holding the requirements file
# and its lockfile, for the scenarios that install from one.
lock_dir() {
  local n="$1" req="$2" prefix="${3:-}" dir="$WORK/lock-${n}${prefix:+-s3}"
  mkdir -p "$dir"
  cp "$req" "$dir/requirements.yml"
  # --no-deps, like every measured command here: what is compared is fetch
  # plus extract over one flat set, not two resolution algorithms.
  env $(gg_env "$prefix") "$GG" lock --no-deps -r "$dir/requirements.yml" >/dev/null 2>&1
  echo "$dir"
}

run_cold() {
  local n="$1" req="$2"
  echo "=== cold cache: requirements-${n}.yml ==="
  hyperfine --runs "$RUNS" \
    --prepare "rm -rf '$TARGET' '$GG_CACHE' '$AG_CACHE'" \
    --export-markdown "$OUT/cold-${n}.md" \
    -n "ansible-galaxy" "env $(ag_env) $AG collection install --no-deps -r $req -p $TARGET" \
    -n "go-galaxy"      "env $(gg_env) $GG install --no-deps -r $req -p $TARGET"
}

run_warm() {
  local n="$1" req="$2"
  echo "=== warm cache: requirements-${n}.yml ==="
  rm -rf "$TARGET" "$GG_CACHE" "$AG_CACHE"
  env $(ag_env) "$AG" collection install --no-deps -r "$req" -p "$TARGET" >/dev/null 2>&1 || true
  rm -rf "$TARGET"
  env $(gg_env) "$GG" install --no-deps -r "$req" -p "$TARGET" >/dev/null 2>&1 || true
  rm -rf "$TARGET"
  hyperfine --runs "$RUNS" --warmup "$WARMUP" \
    --prepare "rm -rf '$TARGET'" \
    --export-markdown "$OUT/warm-${n}.md" \
    -n "ansible-galaxy" "env $(ag_env) $AG collection install --no-deps -r $req -p $TARGET" \
    -n "go-galaxy"      "env $(gg_env) $GG install --no-deps -r $req -p $TARGET"
}

run_frozen() {
  local n="$1" req="$2" dir
  dir="$(lock_dir "$n" "$req")"
  echo "=== frozen+offline: requirements-${n}.yml ==="
  env $(gg_env) "$GG" warm --no-deps -r "$dir/requirements.yml" --frozen >/dev/null 2>&1
  hyperfine --runs "$RUNS" --warmup "$WARMUP" \
    --prepare "rm -rf '$TARGET'" \
    --export-markdown "$OUT/frozen-${n}.md" \
    -n "go-galaxy --frozen --offline" \
    "env $(gg_env) $GG install --no-deps -r $dir/requirements.yml -p $TARGET --frozen --offline"
}

# A fresh prefix per run is what makes s3-cold cold: hyperfine runs one fixed
# command string N times, and the shell it spawns expands this per run, so
# nothing has to be deleted from the bucket between runs.
S3_COLD_PREFIX='cold-$(date +%s)-$$'

run_s3_cold() {
  local n="$1" req="$2"
  echo "=== s3 cold cache: requirements-${n}.yml ==="
  hyperfine --runs "$RUNS" \
    --prepare "rm -rf '$TARGET' '$GG_CACHE'" \
    --export-markdown "$OUT/s3-cold-${n}.md" \
    -n "go-galaxy (s3)" \
    "env $(gg_env "$S3_COLD_PREFIX") $GG install --no-deps -r $req -p $TARGET"
}

run_s3_warm() {
  local n="$1" req="$2" prefix="warm-$n"
  echo "=== s3 warm cache: requirements-${n}.yml ==="
  rm -rf "$TARGET" "$GG_CACHE"
  env $(gg_env "$prefix") "$GG" install --no-deps -r "$req" -p "$TARGET" >/dev/null 2>&1 || true
  hyperfine --runs "$RUNS" --warmup "$WARMUP" \
    --prepare "rm -rf '$TARGET' '$GG_CACHE'" \
    --export-markdown "$OUT/s3-warm-${n}.md" \
    -n "go-galaxy (s3)" \
    "env $(gg_env "$prefix") $GG install --no-deps -r $req -p $TARGET"
}

run_s3_frozen() {
  local n="$1" req="$2" prefix="frozen-$n" dir
  dir="$(lock_dir "$n" "$req" "$prefix")"
  echo "=== s3 frozen: requirements-${n}.yml ==="
  rm -rf "$TARGET" "$GG_CACHE"
  env $(gg_env "$prefix") "$GG" warm --no-deps -r "$dir/requirements.yml" --frozen >/dev/null 2>&1
  hyperfine --runs "$RUNS" --warmup "$WARMUP" \
    --prepare "rm -rf '$TARGET' '$GG_CACHE'" \
    --export-markdown "$OUT/s3-frozen-${n}.md" \
    -n "go-galaxy (s3) --frozen" \
    "env $(gg_env "$prefix") $GG install --no-deps -r $dir/requirements.yml -p $TARGET --frozen"
}

# peak_rss runs one command under /usr/bin/time and prints its peak RSS in
# MiB, to one decimal. The command's own output is discarded; only the
# resource line is read.
peak_rss() {
  local stderr_file="$WORK/.rss.$$"
  if [ "$TIME_FLAVOR" = "bsd" ]; then
    /usr/bin/time -l "$@" >/dev/null 2>"$stderr_file" || true
    awk '/maximum resident set size/ {printf "%.1f", $1/1048576; found=1} END {if (!found) print "n/a"}' "$stderr_file"
  else
    /usr/bin/time -v "$@" >/dev/null 2>"$stderr_file" || true
    awk -F': *' '/Maximum resident set size/ {printf "%.1f", $2/1024; found=1} END {if (!found) print "n/a"}' "$stderr_file"
  fi
  rm -f "$stderr_file"
}

# metric_field prints one field of a go-galaxy metrics report, or n/a.
metric_field() {
  python3 -c '
import json, sys
try:
    with open(sys.argv[1]) as fh:
        print(json.load(fh).get(sys.argv[2], "n/a"))
except Exception:
    print("n/a")
' "$1" "$2"
}

# resource_row runs one command once and appends its markdown row: peak RSS,
# and the bytes its own metrics report says it downloaded.
resource_row() {
  local label="$1" table="$2" metrics="$3"
  shift 3
  rm -f "$metrics"
  local rss bytes
  rss="$(peak_rss "$@")"
  bytes="$(metric_field "$metrics" bytes_downloaded)"
  printf '| %s | %s | %s |\n' "$label" "$rss" "$bytes" >> "$table"
}

run_resources() {
  local n="$1" req="$2" table="$OUT/resources-${n}.md" metrics="$WORK/metrics.json" dir
  echo "=== resources: requirements-${n}.yml ==="
  {
    echo "| Scenario | Peak RSS (MiB) | Bytes downloaded |"
    echo "|:---|---:|---:|"
  } > "$table"

  if scenario_in_list cold; then
    rm -rf "$TARGET" "$GG_CACHE" "$AG_CACHE"
    resource_row "ansible-galaxy, cold" "$table" "$metrics" \
      env $(ag_env) "$AG" collection install --no-deps -r "$req" -p "$TARGET"
    rm -rf "$TARGET" "$GG_CACHE"
    resource_row "go-galaxy, cold" "$table" "$metrics" \
      env $(gg_env) "$GG" install --no-deps -r "$req" -p "$TARGET" --metrics-file "$metrics"
    rm -rf "$TARGET"
    resource_row "ansible-galaxy, warm" "$table" "$metrics" \
      env $(ag_env) "$AG" collection install --no-deps -r "$req" -p "$TARGET"
    rm -rf "$TARGET"
    resource_row "go-galaxy, warm" "$table" "$metrics" \
      env $(gg_env) "$GG" install --no-deps -r "$req" -p "$TARGET" --metrics-file "$metrics"
  fi

  if scenario_in_list frozen; then
    dir="$(lock_dir "$n" "$req")"
    env $(gg_env) "$GG" warm --no-deps -r "$dir/requirements.yml" --frozen >/dev/null 2>&1
    rm -rf "$TARGET"
    resource_row "go-galaxy, frozen+offline" "$table" "$metrics" \
      env $(gg_env) "$GG" install --no-deps -r "$dir/requirements.yml" -p "$TARGET" \
        --frozen --offline --metrics-file "$metrics"
  fi

  if scenario_in_list s3-warm; then
    rm -rf "$TARGET" "$GG_CACHE"
    resource_row "go-galaxy, s3 warm" "$table" "$metrics" \
      env $(gg_env "warm-$n") "$GG" install --no-deps -r "$req" -p "$TARGET" --metrics-file "$metrics"
  fi

  rm -f "$metrics"
}

# ROLES_TARGET is where both tools install roles: ansible-galaxy's role
# install takes it as -p, go-galaxy as --roles-path. ansible-galaxy keeps no
# cache for a role at all, so the roles scenarios wipe only go-galaxy's.
ROLES_TARGET="$WORK/roles"
ROLES_REQ="$ROOT/testing/requirements-roles.yml"

ag_roles_env() {
  printf '%s ' "ANSIBLE_ROLES_PATH=$ROLES_TARGET"
}

run_roles_cold() {
  echo "=== roles, cold cache: requirements-roles.yml ==="
  hyperfine --runs "$RUNS" \
    --prepare "rm -rf '$ROLES_TARGET' '$GG_CACHE'" \
    --export-markdown "$OUT/roles-cold.md" \
    -n "ansible-galaxy" "env $(ag_roles_env) $AG role install --no-deps -r $ROLES_REQ -p $ROLES_TARGET" \
    -n "go-galaxy"      "env $(gg_env) $GG install --no-deps -r $ROLES_REQ --roles-path $ROLES_TARGET"
}

run_roles_warm() {
  echo "=== roles, warm cache: requirements-roles.yml ==="
  rm -rf "$ROLES_TARGET" "$GG_CACHE"
  env $(gg_env) "$GG" install --no-deps -r "$ROLES_REQ" --roles-path "$ROLES_TARGET" >/dev/null 2>&1 || true
  rm -rf "$ROLES_TARGET"
  hyperfine --runs "$RUNS" --warmup "$WARMUP" \
    --prepare "rm -rf '$ROLES_TARGET'" \
    --export-markdown "$OUT/roles-warm.md" \
    -n "ansible-galaxy" "env $(ag_roles_env) $AG role install --no-deps -r $ROLES_REQ -p $ROLES_TARGET" \
    -n "go-galaxy"      "env $(gg_env) $GG install --no-deps -r $ROLES_REQ --roles-path $ROLES_TARGET"
}

main() {
  if ! s3_reachable; then
    for scenario in s3-cold s3-warm s3-frozen; do
      if scenario_in_list "$scenario"; then
        echo "skipping S3 scenarios: $S3_ENDPOINT does not answer" \
             "(start one with: docker compose -f testing/docker-compose.yaml up -d minio-svc)" >&2
        SCENARIOS="$(echo "$SCENARIOS" | sed -e 's/s3-cold//' -e 's/s3-warm//' -e 's/s3-frozen//')"
        break
      fi
    done
  fi

  for n in $SIZES; do
    REQ="$ROOT/testing/requirements-${n}.yml"
    [ -f "$REQ" ] || { echo "missing: $REQ" >&2; exit 1; }
    scenario_in_list cold      && run_cold      "$n" "$REQ"
    scenario_in_list warm      && run_warm      "$n" "$REQ"
    scenario_in_list frozen    && run_frozen    "$n" "$REQ"
    scenario_in_list s3-cold   && run_s3_cold   "$n" "$REQ"
    scenario_in_list s3-warm   && run_s3_warm   "$n" "$REQ"
    scenario_in_list s3-frozen && run_s3_frozen "$n" "$REQ"
    run_resources "$n" "$REQ"
  done

  scenario_in_list roles-cold && run_roles_cold
  scenario_in_list roles-warm && run_roles_warm

  rm -rf "$TARGET" "$ROLES_TARGET"

  {
    echo "# Benchmark summary"
    echo
    echo "Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ) | runs=$RUNS warmup=$WARMUP sizes=\"$SIZES\""
    echo "Host: $(uname -srm)"
    echo "ansible-galaxy: $("$AG" --version 2>/dev/null | head -1)"
    echo "go-galaxy: $("$GG" --version 2>/dev/null | head -1)"
    echo
    for n in $SIZES; do
      echo "## requirements-${n}.yml"
      echo
      for kind in cold warm frozen s3-cold s3-warm s3-frozen; do
        f="$OUT/${kind}-${n}.md"
        if [ -f "$f" ]; then
          echo "### ${kind}"
          echo
          cat "$f"
          echo
        fi
      done
      if [ -f "$OUT/resources-${n}.md" ]; then
        echo "### resources"
        echo
        cat "$OUT/resources-${n}.md"
        echo
      fi
    done
    for kind in roles-cold roles-warm; do
      f="$OUT/${kind}.md"
      if [ -f "$f" ]; then
        echo "## requirements-roles.yml: ${kind#roles-}"
        echo
        cat "$f"
        echo
      fi
    done
  } > "$OUT/summary.md"

  echo
  echo "Done. Summary: $OUT/summary.md"
}

main "$@"
