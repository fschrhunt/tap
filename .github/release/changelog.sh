#!/bin/sh
# Add a release's notes to the top of CHANGELOG.md. The release workflow runs
# this in the release commit, so the file is written only by releases; the
# hand-written Unreleased section from before this flow is dropped.
#
# usage: changelog.sh vX.Y.Z NOTES_FILE
set -eu

[ "$#" -eq 2 ] || { echo 'usage: .github/release/changelog.sh vX.Y.Z NOTES_FILE' >&2; exit 2; }
awk -v entry="## $1 · $(date -u +%Y-%m-%d)" -v notes="$2" '
    function emit() {
        print entry; print ""
        while ((getline line < notes) > 0) print line
        done = 1; gap = 1
    }
    /^## / {
        if (!done) emit()
        skip = ($0 == "## Unreleased")
        if (!skip && gap) { print ""; gap = 0 }
    }
    skip { next }
    { print }
    END { if (!done) emit() }
' CHANGELOG.md > CHANGELOG.md.new
mv CHANGELOG.md.new CHANGELOG.md
