#!/usr/bin/env bash
# CI's sole empty-lock exception: a full pre-release history with no prior lock.
set -euo pipefail
if [ "$(git rev-parse --is-shallow-repository)" != false ]; then
  echo "cannot allow an unpublished image with incomplete git history" >&2
  exit 1
fi
if [ -n "$(git tag --list 'v*')" ]; then
  echo "gateway-image.json cannot be empty after a release tag exists" >&2
  exit 1
fi
# Follow every fetched branch and both parents of merges. Deleting a digest
# before the first tag must not reset the pre-release exception.
for commit in $(git rev-list --all -- gateway-image.json); do
  if git cat-file -e "$commit:gateway-image.json" 2>/dev/null; then
    digest=$(git show "$commit:gateway-image.json" | jq -er '.digest | strings')
    if [ -n "$digest" ]; then
      echo "gateway-image.json cannot be empty: a lock already exists in history at $commit" >&2
      exit 1
    fi
  fi
done
