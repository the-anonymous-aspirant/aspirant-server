# Changelog fragments

`docs/CHANGELOG.md` is not edited in a feature PR. Each PR adds **one file**
here, and `scripts/changelog.sh collect` folds the files into the changelog on
a PR of its own. Two PRs then never touch the same file, so neither conflicts
with the other. Before this, every PR inserted its entry under `## Unreleased`
in the one file, any two open PRs conflicted on the same lines, and each merge
forced a rebase on every sibling (system_3 #9455, #9475).

## Filename

`<YYYY-MM-DD>-<task>[-<slug>].<category>.md`

- `YYYY-MM-DD`: the day the PR is opened.
- `task`: the system_3 task id the PR is linked to, for example `9475`. A PR
  with no task uses its branch slug.
- `slug`: optional, lowercase `a-z0-9-`, when one task opens several PRs on
  the same day.
- `category`: one of `security`, `added`, `changed`, `fixed`, `removed`,
  `deprecated`, the `### <Category>` section of `## Unreleased` the entry
  belongs to. The script creates a missing section in that order.

Example: `2026-11-01-9475.changed.md`

## Body

The entry exactly as it would have gone under that category heading in
`docs/CHANGELOG.md`: one or more `- ` bullets, continuation lines indented two
spaces, past tense, what changed rather than what you did, no heading, no
trailing whitespace, a final newline. The script places it.

    - **Short title** (system_3 #NNNN). What changed, why, and what a caller
      sees now. Keep the git log for the details.

## Commands

    sh scripts/changelog.sh check     # validates the fragments; run before pushing
    sh scripts/changelog.sh render    # prints CHANGELOG.md with the fragments merged in, newest first per category
    sh scripts/changelog.sh collect   # writes that rendering to CHANGELOG.md and deletes the fragments

`collect` runs only on a PR of its own (a chore titled "Collect changelog
fragments"), at most one open at a time. Tests: `sh tests/changelog_unit.sh`.
