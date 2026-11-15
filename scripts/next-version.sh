#!/usr/bin/env bash
# Prints the next release version (vX.Y.Z) for HEAD, worked out from the Conventional Commits since
# the latest vX.Y.Z tag, or prints nothing when HEAD needs no release.
#
#   no tag yet                                   -> v0.1.0
#   breaking change ("type!:" or BREAKING CHANGE) -> major
#   feat                                         -> minor
#   fix, perf, refactor, build, revert, other    -> patch
#   docs, ci, chore, test, style only            -> no release
#
# Merge commits are ignored. HEAD is not released when it is already part of the latest tag.

set -euo pipefail

latest=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n 1)

if [[ -z "$latest" ]]; then
	echo "v0.1.0"
	exit 0
fi

if git merge-base --is-ancestor HEAD "$latest"; then
	exit 0
fi

bump=0 # 0 none, 1 patch, 2 minor, 3 major
type_re='^([a-zA-Z]+)(\([^)]*\))?(!)?: '

while IFS= read -r -d '' msg; do
	subject=${msg%%$'\n'*}
	level=1

	if [[ "$subject" =~ $type_re ]]; then
		type=$(tr '[:upper:]' '[:lower:]' <<<"${BASH_REMATCH[1]}")

		case "$type" in
		feat) level=2 ;;
		docs | ci | chore | test | style) level=0 ;;
		esac

		if [[ -n "${BASH_REMATCH[3]}" ]]; then
			level=3
		fi
	fi

	if grep -qE '^BREAKING[ -]CHANGE: ' <<<"$msg"; then
		level=3
	fi

	if ((level > bump)); then
		bump=$level
	fi
done < <(git log -z --no-merges --format=%B "$latest..HEAD")

if ((bump == 0)); then
	exit 0
fi

IFS=. read -r major minor patch <<<"${latest#v}"

case $bump in
3) echo "v$((major + 1)).0.0" ;;
2) echo "v$major.$((minor + 1)).0" ;;
1) echo "v$major.$minor.$((patch + 1))" ;;
esac
