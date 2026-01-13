#!/usr/bin/env bash

set -euo pipefail

ORIGIN=${ORIGIN:-origin}
MAIN_BRANCH=${MAIN_BRANCH:-main}

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

get_latest_tag() {
    git tag -l 'v*' --sort=-v:refname | head -n 1 || echo "v0.0.0"
}

get_latest_tag_fallback() {
    local tag
    tag=$(get_latest_tag)
    if [[ -z "$tag" ]]; then
        echo "v0.0.0"
    else
        echo "$tag"
    fi
}

echo "Checking out to ${MAIN_BRANCH}..."
git checkout "${MAIN_BRANCH}"

echo "Fetching from ${ORIGIN}..."
git fetch "${ORIGIN}" --tags

echo "Pulling latest changes..."
git pull "${ORIGIN}" "${MAIN_BRANCH}"

if [[ $(git status --porcelain) != "" ]]; then
    echo "Error: repo is dirty. Run git status, clean repo and try again."
    exit 1
fi

current_version=$(get_latest_tag_fallback)
current_version_stripped=${current_version#v}

new_version=$("${DIR}/semver" bump patch "${current_version_stripped}")
new_version="v${new_version}"

echo ""
echo "Current version: ${current_version}"
echo "New version:     ${new_version}"
echo ""

read -p "Create and push tag ${new_version}? [y/N] " -n 1 -r
echo ""

if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Aborted."
    exit 1
fi

git tag -a "${new_version}" -m "Release ${new_version}"
git push "${ORIGIN}" tag "${new_version}"

echo ""
echo "Released ${new_version}"
