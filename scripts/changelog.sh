#!/bin/sh
# scripts/changelog.sh -- per-PR changelog fragments for docs/CHANGELOG.md.
#
# A feature PR never edits docs/CHANGELOG.md. It adds ONE file under
# docs/changelog.d/ (format: docs/changelog.d/README.md). This script folds
# those fragments into the readable changelog:
#
#   sh scripts/changelog.sh check     validate every fragment (name and body)
#   sh scripts/changelog.sh render    print CHANGELOG.md with the fragments merged in
#   sh scripts/changelog.sh collect   write that rendering to CHANGELOG.md and
#                                     delete the fragments
#
# `collect` runs only on a PR of its own, at most one open at a time. Feature
# PRs add fragment files and the collect PR deletes other ones, so no two PRs
# touch the same file. Origin: system_3 #9455 (three verified PRs went
# CONFLICTING on the one shared file in a day).
#
# LAYOUT is the one line that differs between the aspirant repos:
#   flat        bullets straight under "## Unreleased"              (aspirant-client)
#   categories  "### Security|Added|Changed|Fixed|Removed|Deprecated" sections
#               under "## Unreleased"; the category is the fragment name's
#               second suffix, e.g. 2026-11-01-9475.changed.md     (aspirant-server)
#   dates       "### YYYY-MM-DD" headings under the title; the date is the
#               fragment name's prefix                              (aspirant-deploy)
#
# CHANGELOG and FRAGDIR may be overridden from the environment (the unit test
# does); they default to the repo's docs/CHANGELOG.md and docs/changelog.d/.
LAYOUT=categories

set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CHANGELOG=${CHANGELOG:-$ROOT/docs/CHANGELOG.md}
FRAGDIR=${FRAGDIR:-$ROOT/docs/changelog.d}
CATEGORIES="security added changed fixed removed deprecated"

die() { printf 'changelog.sh: %s\n' "$*" >&2; exit 1; }

# Every file in the fragment directory except README.md, newest first (the
# names start with the date, so a reverse name sort is a reverse date sort).
fragments() {
  [ -d "$FRAGDIR" ] || return 0
  ls -1 "$FRAGDIR" | grep -v '^README\.md$' | sort -r || true
}

name_ok() {
  case $LAYOUT in
    categories)
      printf '%s\n' "$1" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}-[a-z0-9][a-z0-9-]*\.(security|added|changed|fixed|removed|deprecated)\.md$' ;;
    *)
      printf '%s\n' "$1" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}-[a-z0-9][a-z0-9-]*\.md$' ;;
  esac
}

name_hint() {
  case $LAYOUT in
    categories) echo '<YYYY-MM-DD>-<task>[-<slug>].<security|added|changed|fixed|removed|deprecated>.md' ;;
    *)          echo '<YYYY-MM-DD>-<task>[-<slug>].md' ;;
  esac
}

frag_category() { printf '%s\n' "$1" | sed -E 's/^.*\.([a-z]+)\.md$/\1/'; }

# Prints every problem with one fragment to stderr; returns 1 if there was any.
check_fragment() {
  b=$1; f=$FRAGDIR/$b; bad=0
  if ! name_ok "$b"; then
    echo "$b: name must be $(name_hint)" >&2; bad=1
  fi
  if [ ! -f "$f" ]; then
    echo "$b: not a regular file" >&2; return 1
  fi
  if [ ! -s "$f" ]; then
    echo "$b: empty" >&2; return 1
  fi
  if ! head -n 1 "$f" | grep -q '^- '; then
    echo "$b: the first line must start with '- '" >&2; bad=1
  fi
  if grep -nE '[[:space:]]+$' "$f" >/dev/null; then
    echo "$b: trailing whitespace on line(s) $(grep -nE '[[:space:]]+$' "$f" | cut -d: -f1 | tr '\n' ' ')" >&2; bad=1
  fi
  if [ "$(tail -c 1 "$f" | od -An -c | tr -d ' ')" != '\n' ]; then
    echo "$b: must end with a newline" >&2; bad=1
  fi
  if grep -n '^#' "$f" >/dev/null; then
    echo "$b: no headings; the script places the entry" >&2; bad=1
  fi
  if ! awk -v b="$b" 'NF && !/^- / && !/^  / { print b ": line " NR " is neither a bullet nor an indented continuation" > "/dev/stderr"; bad = 1 } END { exit bad }' "$f"; then
    bad=1
  fi
  return $bad
}

cmd_check() {
  [ -d "$FRAGDIR" ] || die "$FRAGDIR missing"
  [ -f "$FRAGDIR/README.md" ] || die "$FRAGDIR/README.md missing (the fragment format lives there)"
  rc=0; n=0
  for b in $(fragments); do
    n=$((n + 1))
    if ! check_fragment "$b"; then rc=1; fi
  done
  if [ $rc -eq 0 ]; then echo "changelog.sh: $n fragment(s) ok"; fi
  return $rc
}

# write_block spaced|tight OUT NAME... -- concatenates the named fragments,
# newest first as given; "spaced" puts a blank line after each (flat and
# categories keep a blank line between entries), "tight" does not (dates keeps
# the bullets of one day contiguous).
write_block() {
  style=$1; out=$2; shift 2
  : > "$out"
  for b in "$@"; do
    cat "$FRAGDIR/$b" >> "$out"
    if [ "$style" = spaced ]; then printf '\n' >> "$out"; fi
  done
}

render_flat() {
  write_block spaced "$TMP/block" $(fragments)
  grep -q '^## Unreleased$' "$CHANGELOG" || die "no '## Unreleased' heading in $CHANGELOG"
  awk -v block="$TMP/block" '
    { print }
    !done && /^## Unreleased$/ {
      if ((getline nl) > 0) print nl
      while ((getline l < block) > 0) print l
      close(block)
      done = 1
    }' "$CHANGELOG"
}

render_categories() {
  present=""
  for c in $CATEGORIES; do
    names=$(fragments | grep "\.$c\.md$" || true)
    if [ -n "$names" ]; then
      write_block spaced "$TMP/block.$c" $names
      present="$present $c"
    fi
  done
  grep -q '^## Unreleased$' "$CHANGELOG" || die "no '## Unreleased' heading in $CHANGELOG"
  awk -v tmp="$TMP" -v cats="$CATEGORIES" -v present="$present" '
    BEGIN {
      n = split(cats, order, " ")
      for (i = 1; i <= n; i++) rank[order[i]] = i
      m = split(present, p, " ")
      for (i = 1; i <= m; i++) if (p[i] != "") pending[p[i]] = 1
    }
    function cap(s) { return toupper(substr(s, 1, 1)) substr(s, 2) }
    function emit(c,   l, f) {
      f = tmp "/block." c
      while ((getline l < f) > 0) print l
      close(f)
      delete pending[c]
    }
    # New sections for pending categories ranked below r, in canonical order.
    function emit_missing_before(r,   i) {
      for (i = 1; i <= n; i++)
        if ((order[i] in pending) && rank[order[i]] < r) {
          print "### " cap(order[i]); print ""; emit(order[i])
        }
    }
    /^## Unreleased$/ { inrel = 1; print; next }
    inrel && /^## / { emit_missing_before(n + 1); inrel = 0 }
    inrel && /^### / {
      c = tolower(substr($0, 5))
      if (c in rank) {
        emit_missing_before(rank[c])
        if (c in pending) {
          print
          if ((getline nl) > 0) print nl
          emit(c)
          next
        }
      }
    }
    { print }
    END { if (inrel) emit_missing_before(n + 1) }
  ' "$CHANGELOG"
}

render_dates() {
  dates=$(fragments | cut -c1-10 | uniq)
  for d in $dates; do
    write_block tight "$TMP/block.$d" $(fragments | grep "^$d-")
  done
  awk -v tmp="$TMP" -v dates="$dates" '
    BEGIN { n = split(dates, dl, /[ \n]+/); for (i = 1; i <= n; i++) if (dl[i] != "") pending[dl[i]] = 1 }
    function emit(d,   l, f) {
      f = tmp "/block." d
      while ((getline l < f) > 0) print l
      close(f)
      delete pending[d]
    }
    # New sections for pending dates newer than d, newest first.
    function emit_newer_than(d,   i) {
      for (i = 1; i <= n; i++)
        if ((dl[i] in pending) && dl[i] > d) { print "### " dl[i]; emit(dl[i]); print "" }
    }
    /^### [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]$/ {
      d = substr($0, 5)
      emit_newer_than(d)
      print
      if (d in pending) emit(d)
      next
    }
    { print }
    END {
      for (i = 1; i <= n; i++)
        if (dl[i] in pending) { print ""; print "### " dl[i]; emit(dl[i]) }
    }
  ' "$CHANGELOG"
}

cmd_render() {
  [ -f "$CHANGELOG" ] || die "$CHANGELOG missing"
  cmd_check >/dev/null || die "the fragments fail check; run: $0 check"
  case $LAYOUT in
    flat)       render_flat ;;
    categories) render_categories ;;
    dates)      render_dates ;;
    *)          die "unknown LAYOUT '$LAYOUT' (flat|categories|dates)" ;;
  esac
}

cmd_collect() {
  n=$(fragments | wc -l | tr -d ' ')
  if [ "$n" -eq 0 ]; then
    echo "changelog.sh: no fragments to collect"
    return 0
  fi
  cmd_render > "$TMP/rendered"
  cp "$TMP/rendered" "$CHANGELOG"
  for b in $(fragments); do rm -f "$FRAGDIR/$b"; done
  echo "changelog.sh: folded $n fragment(s) into $CHANGELOG"
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

case ${1:-} in
  check)   cmd_check ;;
  render)  cmd_render ;;
  collect) cmd_collect ;;
  *)       echo "usage: $0 check|render|collect" >&2; exit 2 ;;
esac
