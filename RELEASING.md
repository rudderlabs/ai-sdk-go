# Releasing github.com/rudderlabs/ai-sdk-go

Releases are automated with [release-please](https://github.com/googleapis/release-please). A Go module is published by its git tag: there is no registry upload step.

## One-time setup (repository admins)

1. **GitHub App for release-please.** `release-please.yml` uses a GitHub App token, as in `ai-sdk-node`, so release PRs get CI checks. `vars.RELEASE_APP_CLIENT_ID` and `secrets.RELEASE_PRIVATE_KEY` are available to this repository as organization-level values; the app must also be **installed on this repository** (contents and pull-requests write).
2. **Branch protection** on `main`: require pull requests and the CI checks. Pull requests are squash-merged, so the PR title becomes the commit release-please reads.

## First release (0.1.0)

`release-please-config.json` pins the first release with `"release-as": "0.1.0"` (the manifest starts at `0.0.0`). Remove that pin in a follow-up PR once `v0.1.0` is tagged, so later versions follow the commits.

## How release-please works here

- On every push to `main`, release-please opens or updates a `chore: release x.y.z` PR that bumps `Version` in `version.go` (`x-release-please-version` marker) and the manifest, and updates `CHANGELOG.md`. Before 1.0, `feat` and breaking changes bump the minor version and `fix` the patch version.
- Merging that PR tags `vX.Y.Z` (no component prefix, as Go modules require) and publishes a GitHub release.
- The tag is the release: `go get github.com/rudderlabs/ai-sdk-go@vX.Y.Z` resolves it. While the repository is private, consumers need `GOPRIVATE=github.com/rudderlabs/*` and git credentials for it; once public, the Go module proxy (`proxy.golang.org`) and `pkg.go.dev` pick the tag up on first request.
- Never move or delete a published tag: the Go checksum database records each version's hash, and a changed tag breaks every consumer. Fix forward with a new version, or add a `retract` directive to `go.mod` for a bad one.

If release-please ever starts from the wrong commit, set `"bootstrap-sha"` in `release-please-config.json` to the commit of the last release and remove it after the next release PR.
