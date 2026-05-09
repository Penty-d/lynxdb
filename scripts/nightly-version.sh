#!/bin/sh
set -eu

base_version="${1:-${NIGHTLY_BASE_VERSION:-}}"

if [ -z "$base_version" ]; then
    latest_tag="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)"
    if [ -z "$latest_tag" ]; then
        echo "ERROR: no stable tag found and NIGHTLY_BASE_VERSION is unset" >&2
        exit 1
    fi
    version_no_v="${latest_tag#v}"
    major="${version_no_v%%.*}"
    rest="${version_no_v#*.}"
    minor="${rest%%.*}"
    patch="${rest#*.}"
    patch=$((patch + 1))
    base_version="${major}.${minor}.${patch}"
fi

case "$base_version" in
    v*) base_version="${base_version#v}" ;;
esac

if ! printf '%s\n' "$base_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "ERROR: base version must match X.Y.Z" >&2
    exit 1
fi

if [ "${CI:-}" != "true" ]; then
    if [ -n "$(git status --porcelain)" ]; then
        echo "ERROR: refusing to calculate a local nightly tag from a dirty worktree" >&2
        exit 1
    fi
fi

date_utc="$(date -u +%Y%m%d)"
short_sha="$(git rev-parse --short=7 HEAD)"
tag="v${base_version}-nightly.${date_utc}.g${short_sha}"

printf 'base_version=%s\n' "$base_version"
printf 'date_utc=%s\n' "$date_utc"
printf 'short_sha=%s\n' "$short_sha"
printf 'tag=%s\n' "$tag"
