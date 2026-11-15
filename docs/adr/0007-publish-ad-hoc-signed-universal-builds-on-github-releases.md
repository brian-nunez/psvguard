---
status: accepted
date: 2026-11-14
decision-makers: Brian Nunez
---

# Publish ad-hoc signed universal builds on GitHub Releases

## Context and Problem Statement

[ADR-0005](0005-ship-macos-builds-as-an-ad-hoc-signed-app-bundle.md) packages PSV Guard as an ad-hoc signed `.app` built from source on Brian's Mac, with no distribution, and says distributing it would need a new ADR. Building from source needs Go, the Xcode Command Line Tools and the repository, which keeps PSV Guard out of reach of most people it is meant for. psv, which PSV Guard is built on, already releases automatically from GitHub Actions.

How are PSV Guard builds released to other Macs, without an Apple Developer account or any secrets?

## Decision Drivers

* Anyone on macOS 14 or later, on Apple silicon or Intel, can download and run PSV Guard.
* No Apple Developer account, certificates or repository secrets.
* Releases are reproducible from a commit and never made from a local machine.
* The same versioning and release flow as psv.
* Screen Recording permission still belongs to PSV Guard's stable bundle ID ([ADR-0005](0005-ship-macos-builds-as-an-ad-hoc-signed-app-bundle.md)).

## Considered Options

* Ad-hoc signed universal app, zipped, on GitHub Releases, released by GitHub Actions
* Developer ID signed and notarised app
* No published builds: source only, with CI artifacts for testing

## Decision Outcome

Chosen option: "Ad-hoc signed universal app, zipped, on GitHub Releases, released by GitHub Actions", because it puts a native app in people's hands with nothing to sign into and no secrets to keep, at the cost of one Gatekeeper prompt on first launch.

* `make release VERSION=vX.Y.Z` builds `psvguard` for `arm64` and `amd64` with cgo, joins them with `lipo`, assembles and ad-hoc signs `PSV Guard.app` as `make app` does, zips it with `ditto` into `dist/PSV-Guard-macos.zip` and writes `dist/SHA256SUMS`.
* The archive name stays the same in every release, so `releases/latest/download/PSV-Guard-macos.zip` always works.
* The CI workflow runs on every pull request and push to `main`. When it passes on `main`, the Release workflow works out the next version from the Conventional Commits since the last tag with `scripts/next-version.sh`, as psv does, builds and verifies the archive on macOS and publishes it as a GitHub release. No tag or release is made by hand.
* The bundle's `CFBundleShortVersionString` and `CFBundleVersion` are the tag without its leading `v`.
* The README tells downloaders how to open the app the first time.

### Consequences

* Good, because PSV Guard can be installed without a toolchain, on Apple silicon and Intel Macs.
* Good, because there is no account, certificate or secret to manage or leak; the workflow needs only `contents: write`.
* Good, because every release is built from a commit that passed CI, and its checksum is published.
* Bad, because the app is not notarised: macOS blocks the first launch of the downloaded app, and the user must choose Open Anyway in System Settings, Privacy & Security, once.
* Bad, because each build's ad-hoc signature differs, so macOS may ask for Screen Recording permission again after an update, as after a local rebuild.
* Neutral, because merging a `feat` or `fix` to `main` now publishes a release; `docs`, `ci`, `chore`, `test` and `style` commits alone do not.

### Confirmation

* The CI workflow runs `make release` and verifies the signature and checksums on every pull request.
* The Release workflow unzips the archive, verifies its signature, checks that the binary holds `x86_64` and `arm64`, and checks that it reports the release's version before publishing.
* `scripts/next-version_test.sh`, run by `make test-scripts` in CI, covers the version rules.

## Pros and Cons of the Options

### Ad-hoc signed universal app, zipped, on GitHub Releases, released by GitHub Actions

* Good, because it needs only the tools on GitHub's macOS runners.
* Good, because it reuses `make app` and psv's proven release flow.
* Bad, because of the first-launch Gatekeeper prompt.

### Developer ID signed and notarised app

* Good, because it opens without a Gatekeeper prompt, and its stable signing identity keeps Screen Recording permission across updates.
* Bad, because it needs a paid Apple Developer account, a certificate and notarisation credentials stored as repository secrets.

### No published builds: source only, with CI artifacts for testing

* Good, because nothing is distributed and ADR-0005 stands unchanged.
* Bad, because only people who can build Go from source can use PSV Guard.

## More Information

* This record narrows ADR-0005's "no distribution": builds are still ad-hoc signed `.app` bundles with the stable bundle ID `com.briannunez.psvguard`.
* Revisit for notarisation, auto-update or launch at login; each needs its own record.
