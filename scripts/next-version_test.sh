#!/usr/bin/env bash
# Tests for next-version.sh, run against throwaway Git repositories. Run with `make test-scripts`.

set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/next-version.sh"
failures=0

repo() {
	dir=$(mktemp -d)
	git -C "$dir" init -q -b main
	git -C "$dir" config user.name test
	git -C "$dir" config user.email test@example.com
	git -C "$dir" config commit.gpgsign false
	git -C "$dir" config tag.gpgsign false
}

commit() {
	git -C "$dir" commit -q --allow-empty -m "$1"
}

expect() {
	local name=$1 want=$2 got
	got=$(cd "$dir" && bash "$script")

	if [[ "$got" == "$want" ]]; then
		echo "ok   $name"
	else
		echo "FAIL $name: want '$want', got '$got'"
		failures=$((failures + 1))
	fi

	rm -rf "$dir"
}

repo
commit "feat: first"
expect "first release" "v0.1.0"

repo
commit "feat: first"
git -C "$dir" tag v0.1.0
commit "fix(vad): keep lead-in"
expect "fix bumps patch" "v0.1.1"

repo
commit "feat: first"
git -C "$dir" tag v0.1.3
commit "fix: a"
commit "feat(ui): b"
expect "feat bumps minor" "v0.2.0"

repo
commit "feat: first"
git -C "$dir" tag v0.2.0
commit "feat(app)!: drop flag"
expect "bang bumps major" "v1.0.0"

repo
commit "feat: first"
git -C "$dir" tag v1.4.2
commit $'refactor: store\n\nBREAKING CHANGE: new schema'
expect "breaking footer bumps major" "v2.0.0"

repo
commit "feat: first"
git -C "$dir" tag v0.1.0
commit "docs: readme"
commit "ci: workflow"
commit "chore: tidy"
expect "docs, ci and chore only skip release" ""

repo
commit "feat: first"
git -C "$dir" tag v0.1.0
commit "Update things"
expect "non-conventional subject bumps patch" "v0.1.1"

repo
commit "feat: first"
git -C "$dir" tag v0.1.0
expect "tagged HEAD skips release" ""

repo
commit "feat: first"
git -C "$dir" tag v0.9.0
git -C "$dir" tag v0.10.0
commit "fix: a"
expect "tags sort by version" "v0.10.1"

if ((failures > 0)); then
	echo "$failures failed"
	exit 1
fi
