#!/usr/bin/env bash
# Is the commit being built main's head right now? (system_3 #9966)
#
# The Build Image workflow pushes the moving tags (:latest, :main) only when
# this prints `is_head=true`. On 2026-11-05 five merges landed within 90s,
# 32f030a's build finished last, and :latest went back to it, so the served
# site lost #404 and #405. The workflow's concurrency group now orders the
# pushes, but a re-run of an older run would still retag :latest with that
# older commit and roll the site back. Asking the remote at push time closes
# that hole: an older commit is never main's head.
#
# Usage: is-main-head.sh <sha> [remote]   (remote defaults to origin)
# Prints exactly one line to stdout, `is_head=true` or `is_head=false`, for
# "$GITHUB_OUTPUT". Exits non-zero (and prints nothing to stdout) when main's
# head cannot be read, so the build fails rather than guessing either way.
# Unit test: sh tests/is_main_head_unit.sh
set -euo pipefail

sha=${1:?usage: is-main-head.sh <sha> [remote]}
remote=${2:-origin}

if ! listing=$(git ls-remote "$remote" refs/heads/main); then
  echo "ERROR: could not read main's head from '$remote'" >&2
  exit 1
fi
# ls-remote's pattern matches as a path suffix, so a branch named e.g.
# `a/refs/heads/main` is listed too, and can sort first. Take only the line
# whose ref is exactly refs/heads/main (security review c61631 on #9966).
head=$(printf '%s\n' "$listing" | awk -F'\t' '$2 == "refs/heads/main" { print $1 }')
if ! [[ $head =~ ^[0-9a-f]{40}$ ]]; then
  echo "ERROR: '$remote' returned no usable head for refs/heads/main" >&2
  exit 1
fi

if [ "$head" = "$sha" ]; then
  echo "is_head=true"
else
  echo "main is at $head, this build is $sha: pushing only its sha- tag, not :latest or :main" >&2
  echo "is_head=false"
fi
