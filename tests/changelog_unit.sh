#!/bin/sh
# tests/changelog_unit.sh -- unit test for scripts/changelog.sh (sh only, no
# network, no node). Run: sh tests/changelog_unit.sh   (sh tests/changelog_unit.sh)
#
# Covers the three layouts the aspirant repos use (flat, categories, dates),
# render byte-equality when there are no fragments, collect idempotency and
# every refusal of `check`. Fixtures are built inline below. system_3 #9455.
set -u

HERE=$(cd "$(dirname "$0")/.." && pwd)
SCRIPT=$HERE/scripts/changelog.sh
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
pass=0; fail=0

ok()  { pass=$((pass + 1)); echo "ok   - $1"; }
nok() { fail=$((fail + 1)); echo "FAIL - $1"; }

# variant LAYOUT -> path of a copy of the script with that LAYOUT line.
variant() {
  sed "s/^LAYOUT=.*/LAYOUT=$1/" "$SCRIPT" > "$T/changelog-$1.sh"
  echo "$T/changelog-$1.sh"
}

# fixture NAME -> creates $T/NAME/{CHANGELOG.md from stdin, changelog.d/README.md}
fixture() {
  mkdir -p "$T/$1/changelog.d"
  cat > "$T/$1/CHANGELOG.md"
  echo '# fragments' > "$T/$1/changelog.d/README.md"
}

# frag FIXTURE NAME -> writes stdin to $T/FIXTURE/changelog.d/NAME
frag() { cat > "$T/$1/changelog.d/$2"; }

# run FIXTURE LAYOUT VERB -> runs the verb against the fixture; stdout to
# $T/out, stderr to $T/err; returns the verb's exit code.
run() {
  CHANGELOG=$T/$1/CHANGELOG.md FRAGDIR=$T/$1/changelog.d sh "$(variant "$2")" "$3" > "$T/out" 2> "$T/err"
}

assert_eq_file() { # NAME EXPECTED ACTUAL
  if diff -u "$2" "$3" > "$T/diff.out"; then ok "$1"; else nok "$1"; cat "$T/diff.out"; fi
}

assert_fails_with() { # NAME SUBSTRING  (after a run that should have failed)
  if [ "$rc" -ne 0 ] && grep -q "$2" "$T/err"; then ok "$1"; else nok "$1 (rc=$rc)"; cat "$T/err"; fi
}

# ---------------------------------------------------------------- flat -----
fixture flat <<'EOF'
# Changelog

## Unreleased

- Old entry one (system_3 #1).
  Continues.

- Old entry two.

## 2026-03-10

- Extracted.
EOF

run flat flat render; rc=$?
assert_eq_file "flat: render with no fragments is byte-identical" "$T/flat/CHANGELOG.md" "$T/out"
[ "$rc" -eq 0 ] && ok "flat: render exit 0" || nok "flat: render exit $rc"

frag flat 2026-10-31-100.md <<'EOF'
- New Oct 31.
EOF
frag flat 2026-11-01-200.md <<'EOF'
- New Nov 1.
  Second line.
EOF
cat > "$T/flat.expected" <<'EOF'
# Changelog

## Unreleased

- New Nov 1.
  Second line.

- New Oct 31.

- Old entry one (system_3 #1).
  Continues.

- Old entry two.

## 2026-03-10

- Extracted.
EOF
run flat flat render; rc=$?
assert_eq_file "flat: two fragments land under Unreleased, newest first" "$T/flat.expected" "$T/out"

run flat flat check; rc=$?
[ "$rc" -eq 0 ] && grep -q '2 fragment(s) ok' "$T/out" && ok "flat: check counts 2 ok" || nok "flat: check (rc=$rc) $(cat "$T/out" "$T/err")"

# collect: writes the rendering, deletes the fragments, keeps README, idempotent
run flat flat collect; rc=$?
assert_eq_file "collect: CHANGELOG.md holds the rendering" "$T/flat.expected" "$T/flat/CHANGELOG.md"
[ "$(ls "$T/flat/changelog.d")" = "README.md" ] && ok "collect: only README.md is left" || nok "collect: left $(ls "$T/flat/changelog.d")"
run flat flat collect; rc=$?
grep -q 'no fragments to collect' "$T/out" && ok "collect: second run is a no-op" || nok "collect: second run said $(cat "$T/out")"
assert_eq_file "collect: second run leaves the file byte-identical" "$T/flat.expected" "$T/flat/CHANGELOG.md"

# ---------------------------------------------------------- categories -----
fixture cats <<'EOF'
# Changelog

## Unreleased

### Security

- Sec newer.

### Added

- Added old.

### Fixed

- Fixed old.

### Security

- Sec older.

## v1.0.0

### Included

- Thing.
EOF
frag cats 2026-11-01-300.added.md <<'EOF'
- Added new.
EOF
frag cats 2026-11-01-301.removed.md <<'EOF'
- Removed new.
EOF
frag cats 2026-10-30-302.changed.md <<'EOF'
- Changed new.
EOF
cat > "$T/cats.expected" <<'EOF'
# Changelog

## Unreleased

### Security

- Sec newer.

### Added

- Added new.

- Added old.

### Changed

- Changed new.

### Fixed

- Fixed old.

### Security

- Sec older.

### Removed

- Removed new.

## v1.0.0

### Included

- Thing.
EOF
run cats categories render; rc=$?
assert_eq_file "categories: existing section, created sections in rank order, first Security kept" "$T/cats.expected" "$T/out"

mkdir -p "$T/catname/changelog.d"; cp "$T/cats/CHANGELOG.md" "$T/catname/CHANGELOG.md"; echo x > "$T/catname/changelog.d/README.md"
frag catname 2026-11-01-600.md <<'EOF'
- No category suffix.
EOF
run catname categories check; rc=$?
assert_fails_with "categories: check refuses a name without a category" 'name must be'

# --------------------------------------------------------------- dates -----
fixture dates <<'EOF'
# Changelog

### 2026-10-28
- Old a.
  - sub.
- Old b.

### 2026-10-25
- Older.
EOF
frag dates 2026-10-28-400.md <<'EOF'
- New for 28.
EOF
frag dates 2026-11-01-401.md <<'EOF'
- Newest.
EOF
frag dates 2026-11-01-402.md <<'EOF'
- Newest two.
EOF
frag dates 2026-10-20-403.md <<'EOF'
- Oldest.
EOF
cat > "$T/dates.expected" <<'EOF'
# Changelog

### 2026-11-01
- Newest two.
- Newest.

### 2026-10-28
- New for 28.
- Old a.
  - sub.
- Old b.

### 2026-10-25
- Older.

### 2026-10-20
- Oldest.
EOF
run dates dates render; rc=$?
assert_eq_file "dates: existing day, new newest day on top, new oldest day at the end" "$T/dates.expected" "$T/out"

# --------------------------------------------------------------- check -----
checkcase() { # NAME FILENAME SUBSTRING BODY (printf %b escapes; no pipe, so the counters survive)
  fixture "chk-$1" <<'EOF'
# Changelog

## Unreleased

EOF
  printf '%b' "$4" > "$T/chk-$1/changelog.d/$2"
  run "chk-$1" flat check; rc=$?
  assert_fails_with "check: refuses $1" "$3"
}
checkcase badname notadate-1.md 'name must be' '- fine\n'
checkcase nobullet 2026-11-01-500.md "first line must start with '- '" 'No bullet\n'
checkcase trailing 2026-11-01-501.md 'trailing whitespace' '- trailing \n'
checkcase nonewline 2026-11-01-502.md 'must end with a newline' '- no newline'
checkcase heading 2026-11-01-503.md 'no headings' '- ok\n### Heading\n'
checkcase continuation 2026-11-01-504.md 'neither a bullet nor an indented continuation' '- ok\nnot indented\n'

fixture good <<'EOF'
# Changelog

## Unreleased

EOF
frag good 2026-11-01-505.md <<'EOF'
- good
  continued

- second bullet
EOF
run good flat check; rc=$?
[ "$rc" -eq 0 ] && grep -q '1 fragment(s) ok' "$T/out" && ok "check: accepts a good fragment" || nok "check: good fragment rc=$rc $(cat "$T/err")"

run good flat render; rc=$?
cat > "$T/good.expected" <<'EOF'
# Changelog

## Unreleased

- good
  continued

- second bullet

EOF
assert_eq_file "render: a fragment on an empty Unreleased" "$T/good.expected" "$T/out"

# render refuses when a fragment fails check
mkdir -p "$T/refuse/changelog.d"; cp "$T/good/CHANGELOG.md" "$T/refuse/"; echo x > "$T/refuse/changelog.d/README.md"
printf 'bad\n' > "$T/refuse/changelog.d/2026-11-01-506.md"
run refuse flat render; rc=$?
assert_fails_with "render: refuses while a fragment fails check" 'fail check'

# ------------------------------------------------------- the real tree -----
if sh "$SCRIPT" check > "$T/out" 2> "$T/err"; then ok "repo: check passes on docs/changelog.d ($(cat "$T/out"))"; else nok "repo: check failed: $(cat "$T/err")"; fi

echo "changelog_unit: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
