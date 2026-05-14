#!/usr/bin/env bash
set -euo pipefail

ROOT="${1:-/home/axx_0213/598Project/seaweed-mnt/_sent_mail}"
TAGCTL_BIN="${TAGCTL_BIN:-/home/axx_0213/598Project/tagindex/bin/tagctl}"
TAG_OP="${TAG_OP:-set}"
TAGS_PER_FILE="${TAGS_PER_FILE:-4}"
TAG_COUNT="${TAG_COUNT:-20}"
DRY_RUN="${DRY_RUN:-0}"

if [[ "$TAG_OP" != "set" && "$TAG_OP" != "add" ]]; then
  echo "TAG_OP must be set or add, got: $TAG_OP" >&2
  exit 2
fi

if [[ ! -x "$TAGCTL_BIN" ]]; then
  echo "tagctl binary is not executable: $TAGCTL_BIN" >&2
  exit 2
fi

if [[ ! -d "$ROOT" ]]; then
  echo "root directory does not exist: $ROOT" >&2
  exit 2
fi

if (( TAGS_PER_FILE < 1 || TAGS_PER_FILE > TAG_COUNT )); then
  echo "TAGS_PER_FILE must be between 1 and TAG_COUNT" >&2
  exit 2
fi

pick_tags() {
  shuf -i "1-${TAG_COUNT}" -n "$TAGS_PER_FILE" |
    sed 's/^/random_tag_/' |
    paste -sd, -
}

count=0
echo "root: $ROOT"
echo "tagctl: $TAGCTL_BIN"
echo "operation: $TAG_OP"
echo "tags per file: $TAGS_PER_FILE from random_tag_1..random_tag_${TAG_COUNT}"

while IFS= read -r -d '' file; do
  tags="$(pick_tags)"
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '%s %q %s\n' "$TAG_OP" "$file" "$tags"
  else
    "$TAGCTL_BIN" "$TAG_OP" "$file" "$tags" >/dev/null
  fi

  count=$((count + 1))
  if (( count % 100 == 0 )); then
    echo "tagged $count file(s)..."
  fi
done < <(find "$ROOT" -type f -print0)

echo "done: tagged $count file(s)"
