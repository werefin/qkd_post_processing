#!/usr/bin/env bash
# Tags a release from main: checks the tree, runs the tests, and pushes a semver tag; publishing stays a separate,
# deliberate step --> scripts/release.sh patch|minor|major|vX.Y.Z, then gh workflow run release.yml -f tag=<tag>
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

bump="${1:-}"
[[ -n "$bump" ]] || { echo "usage: scripts/release.sh patch|minor|major|vX.Y.Z" >&2; exit 2; }

[[ "$(git rev-parse --abbrev-ref HEAD)" == main ]] || { echo "release from main only" >&2; exit 1; }
[[ -z "$(git status --porcelain)" ]] || { echo "the working tree has changes: commit or stash them first" >&2; exit 1; }
git fetch --quiet --tags origin
[[ "$(git rev-parse HEAD)" == "$(git rev-parse origin/main)" ]] || { echo "main differs from origin/main: pull or push first" >&2; exit 1; }

last="$(git tag --list 'v*.*.*' --sort=-v:refname | head -n1)"
last="${last:-v0.0.0}"
IFS=. read -r major minor patch <<<"${last#v}"
case "$bump" in
patch) next="v$major.$minor.$((patch + 1))" ;;
minor) next="v$major.$((minor + 1)).0" ;;
major) next="v$((major + 1)).0.0" ;;
v*.*.*) next="$bump" ;;
*) echo "patch, minor, major or vX.Y.Z" >&2; exit 2 ;;
esac
[[ "$next" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "$next is not vX.Y.Z" >&2; exit 2; }
git rev-parse -q --verify "refs/tags/$next" >/dev/null && { echo "$next already exists" >&2; exit 1; }

echo "tests before tagging $next (last release $last)"
go vet ./...
go test -count=1 ./...
python3 -m unittest services.tests.test_services

echo
git log --oneline "${last}..HEAD" 2>/dev/null || git log --oneline -20
read -r -p "tag and push $next? [y/N] " ok
[[ "$ok" == y || "$ok" == Y ]] || { echo "nothing tagged"; exit 0; }
git tag -a "$next" -m "$next"
git push origin "$next"
echo "$next pushed; to publish its images and release: gh workflow run release.yml -f tag=$next"
