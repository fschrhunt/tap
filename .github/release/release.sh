#!/bin/sh
# Releases tap from an up-to-date main checkout:
#
#   .github/release/release.sh v1.0.0
#
# Drafts the notes (the pull requests merged since the last release) and opens
# them in your editor; what you save becomes the GitHub release and its
# CHANGELOG.md entry. Then, once CI has passed, it asks Actions to release:
# the workflow adds the notes to CHANGELOG.md in a release commit on top of
# main, tags it, opens a pull request that brings the changelog to main, and
# the tag starts the build. Needs git and gh. Your checkout stays on main.
set -eu

fail() { echo "release: $*" >&2; exit 1; }
tag=${1:-}
case $tag in v[0-9]*.[0-9]*.[0-9]*) ;; *) fail "usage: .github/release/release.sh vX.Y.Z" ;; esac
command -v gh >/dev/null 2>&1 || fail "needs gh, the GitHub CLI"
cd "$(git rev-parse --show-toplevel)"

[ "$(git rev-parse --abbrev-ref HEAD)" = main ] || fail "run it on main"
[ -z "$(git status --porcelain)" ] || fail "commit or set aside your changes first"
git fetch -q origin main --tags
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || fail "main is not origin/main; pull first"
git rev-parse -q --verify "refs/tags/$tag" >/dev/null && fail "$tag already exists"

commit=$(git rev-parse HEAD)
result=$(gh run list --workflow ci.yml --commit "$commit" --json status,conclusion -q '.[0] | "\(.status) \(.conclusion)"')
case $result in
  "completed success") ;;
  "") fail "CI has not run on $commit yet" ;;
  *) fail "CI on $commit is \"$result\"; release once it passes" ;;
esac

# The draft: notes still under Unreleased from before releases wrote the
# changelog, otherwise the PR titles merged since the last release. Release
# tags sit on their own commit beside main, so count from where it branched.
notes=$(mktemp "${TMPDIR:-/tmp}/release-notes.XXXXXX")
trap 'rm -f "$notes"' EXIT
awk '/^## / { s = ($0 == "## Unreleased"); next } s' CHANGELOG.md | sed '/./,$!d' > "$notes"
if [ ! -s "$notes" ]; then
  last=$(git tag --list 'v*' --sort=-v:refname | head -n 1)
  range=HEAD
  [ -z "$last" ] || range="$(git merge-base "$last" HEAD)..HEAD"
  git log --no-merges --invert-grep --grep='^release: ' --format='- %s' "$range" > "$notes"
fi
${VISUAL:-${EDITOR:-vi}} "$notes"
grep -q '[^[:space:]]' "$notes" || fail "the release notes are empty"

gh workflow run release.yml --ref main -f "tag=$tag" -f "notes=$(cat "$notes")"
echo "Asked Actions to release $tag. Watch with: gh run watch \$(gh run list --workflow release.yml -L 1 --json databaseId -q '.[0].databaseId')"
