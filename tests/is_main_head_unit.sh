#!/bin/sh
# tests/is_main_head_unit.sh -- unit test for scripts/is-main-head.sh (no
# network: the "remote" is a local bare repo). Run: sh tests/is_main_head_unit.sh
# system_3 #9966 (copied from aspirant-client e309472 for #9982).
set -u

HERE=$(cd "$(dirname "$0")/.." && pwd)
SCRIPT=$HERE/scripts/is-main-head.sh
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
pass=0; fail=0

ok()  { pass=$((pass + 1)); echo "ok   - $1"; }
nok() { fail=$((fail + 1)); echo "FAIL - $1"; }

# A bare "remote" whose main has two commits: old, then head.
git init -q -b main "$T/work"
git -C "$T/work" -c user.name=t -c user.email=t@t commit -q --allow-empty -m old
old=$(git -C "$T/work" rev-parse HEAD)
git -C "$T/work" -c user.name=t -c user.email=t@t commit -q --allow-empty -m head
head=$(git -C "$T/work" rev-parse HEAD)
git clone -q --bare "$T/work" "$T/remote.git"

out=$(bash "$SCRIPT" "$head" "$T/remote.git" 2>/dev/null); rc=$?
[ "$rc" = 0 ] && [ "$out" = "is_head=true" ] && ok "main's head -> is_head=true" || nok "main's head -> got rc=$rc out='$out'"

out=$(bash "$SCRIPT" "$old" "$T/remote.git" 2>/dev/null); rc=$?
[ "$rc" = 0 ] && [ "$out" = "is_head=false" ] && ok "an older commit (a re-run) -> is_head=false" || nok "older commit -> got rc=$rc out='$out'"

err=$(bash "$SCRIPT" "$old" "$T/remote.git" 2>&1 >/dev/null)
case $err in *"main is at $head"*) ok "an older commit says which head it lost to" ;; *) nok "older commit stderr: '$err'" ;; esac

out=$(bash "$SCRIPT" "$head" "$T/no-such-remote.git" 2>/dev/null); rc=$?
[ "$rc" != 0 ] && [ -z "$out" ] && ok "an unreadable remote fails the build and writes no output" || nok "unreadable remote -> got rc=$rc out='$out'"

# A branch whose name ends in refs/heads/main, left on the older commit: the
# suffix match would list it too, and it sorts before refs/heads/main.
git clone -q --bare "$T/work" "$T/decoy.git"
git -C "$T/decoy.git" update-ref refs/heads/a/refs/heads/main "$old"
out=$(bash "$SCRIPT" "$head" "$T/decoy.git" 2>/dev/null); rc=$?
out_old=$(bash "$SCRIPT" "$old" "$T/decoy.git" 2>/dev/null)
[ "$rc" = 0 ] && [ "$out" = "is_head=true" ] && [ "$out_old" = "is_head=false" ] \
  && ok "a branch named a/refs/heads/main does not stand in for main" \
  || nok "decoy branch -> head build '$out' (rc=$rc), older build '$out_old'"

git init -q --bare "$T/empty.git"
out=$(bash "$SCRIPT" "$head" "$T/empty.git" 2>/dev/null); rc=$?
[ "$rc" != 0 ] && [ -z "$out" ] && ok "a remote with no main fails the build" || nok "no main -> got rc=$rc out='$out'"

out=$(bash "$SCRIPT" 2>/dev/null); rc=$?
[ "$rc" != 0 ] && [ -z "$out" ] && ok "no sha argument fails" || nok "no sha -> got rc=$rc out='$out'"

echo "is_main_head_unit: $pass passed, $fail failed"
[ "$fail" = 0 ]
