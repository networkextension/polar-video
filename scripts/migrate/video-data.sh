#!/usr/bin/env bash
# video-data.sh — copy video_* tables from ideamesh → polar_video.
# Same shape as scripts/migrate/{wg,iosdist}-data.sh.

set -euo pipefail

SRC_DSN="${SRC_DSN:-postgres://ideamesh:test123456@127.0.0.1:5432/ideamesh}"
DST_DSN="${DST_DSN:-postgres://ideamesh:test123456@127.0.0.1:5432/polar_video}"
PSQL="${PSQL:-/Applications/Postgres.app/Contents/Versions/latest/bin/psql}"
PG_DUMP="${PG_DUMP:-/Applications/Postgres.app/Contents/Versions/latest/bin/pg_dump}"

APPLY=0
if [[ "${1:-}" == "--apply" ]]; then
    APPLY=1
fi

TABLES=(video_projects video_shots video_assets)

echo "=== video-data.sh — $(if [[ $APPLY -eq 1 ]]; then echo APPLY; else echo DRY-RUN; fi) ==="
echo "source: $SRC_DSN"
echo "target: $DST_DSN"
echo
echo "--- source row counts ---"
for t in "${TABLES[@]}"; do
    n=$("$PSQL" "$SRC_DSN" -At -c "SELECT COUNT(*) FROM $t;" 2>/dev/null || echo "ERR")
    printf "  %-20s %s\n" "$t" "$n"
done
echo

if [[ $APPLY -eq 0 ]]; then
    echo "Dry run — pass --apply to perform the copy."
    echo "Pre-flight: ensure scripts/migrate/video-schema.sql is applied to target."
    exit 0
fi

TMPDIR=$(mktemp -d -t videomigrate)
trap 'rm -rf "$TMPDIR"' EXIT

DUMP="$TMPDIR/video-data.sql"
"$PG_DUMP" "$SRC_DSN" \
    --data-only \
    --column-inserts \
    --no-owner \
    --no-privileges \
    $(printf -- '--table=%s ' "${TABLES[@]}") \
    > "$DUMP"
echo "wrote $(wc -l < "$DUMP") lines to $DUMP"

{
    echo "BEGIN;"
    for t in "${TABLES[@]}"; do
        echo "TRUNCATE $t RESTART IDENTITY CASCADE;"
    done
    cat "$DUMP"
    echo "COMMIT;"
} | "$PSQL" "$DST_DSN" -v ON_ERROR_STOP=1

echo
echo "--- target row counts (post-load) ---"
for t in "${TABLES[@]}"; do
    n=$("$PSQL" "$DST_DSN" -At -c "SELECT COUNT(*) FROM $t;")
    printf "  %-20s %s\n" "$t" "$n"
done
echo "Done. video-svc can now point POLAR_VIDEO_DB_DSN at $DST_DSN."
