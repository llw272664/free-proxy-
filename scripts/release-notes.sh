#!/bin/sh
# Render the body of a GitHub release: every commit since the previous tag,
# grouped by what kind of change it was.
#
# GitHub's own generated notes list merged pull requests, and this project
# commits straight to main — so what it generates is an empty page with a
# compare link, and an operator deciding whether to press "更新" in the console
# is told nothing. These notes are that decision's only input, so they are built
# from the commits themselves.
#
# The headings match internal/updater/release.go, which parses them back out of
# the published body for the console to render.
#
# Usage: scripts/release-notes.sh <tag> [previous-tag]

set -eu

tag=${1:?usage: release-notes.sh <tag> [previous-tag]}
prev=${2:-$(git describe --tags --abbrev=0 "$tag^" 2>/dev/null || true)}

if [ -n "$prev" ]; then
    range="$prev..$tag"
else
    range="$tag"
fi

# GITHUB_REPOSITORY is set by Actions; outside it, read the origin remote so the
# script is runnable locally to preview a release.
repo=${GITHUB_REPOSITORY:-}
if [ -z "$repo" ]; then
    # Take the trailing owner/name from whatever form the remote has — an https
    # URL, an scp-style address, or a host alias from ~/.ssh/config.
    repo=$(git remote get-url origin 2>/dev/null |
        sed -e 's#\.git$##' -e 's#^.*[:/]\([^/:]*/[^/]*\)$#\1#' || true)
fi

git log --no-merges --pretty=format:%s "$range" | awk '
# section maps a conventional-commit type onto a heading. An empty answer means
# "not a type this project uses", and the subject is kept whole.
function section(type) {
    if (type == "feat")     return "新功能"
    if (type == "fix")      return "问题修复"
    if (type == "perf")     return "性能优化"
    if (type == "refactor") return "结构调整"
    if (type == "docs")     return "文档"
    if (type == "chore" || type == "build" || type == "ci" ||
        type == "style" || type == "test" || type == "revert") return "其他改动"
    return ""
}
{
    subject = $0
    text = subject
    heading = ""
    if (match(subject, /^[a-zA-Z]+(\([^)]*\))?!?: /)) {
        head = substr(subject, 1, RLENGTH)
        sub(/[(!:].*/, "", head)
        heading = section(tolower(head))
        if (heading != "") text = substr(subject, RLENGTH + 1)
    }
    if (heading == "") heading = "其他改动"
    if (text == "") next
    items[heading] = items[heading] "- " text "\n"
}
END {
    n = split("新功能,问题修复,性能优化,结构调整,文档,其他改动", order, ",")
    for (i = 1; i <= n; i++)
        if (items[order[i]] != "")
            printf "## %s\n%s\n", order[i], items[order[i]]
}
'

if [ -n "$repo" ]; then
    if [ -n "$prev" ]; then
        printf '**Full Changelog**: https://github.com/%s/compare/%s...%s\n' "$repo" "$prev" "$tag"
    else
        printf '**Full Changelog**: https://github.com/%s/commits/%s\n' "$repo" "$tag"
    fi
fi
