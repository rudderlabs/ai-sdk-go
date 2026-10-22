# Releasing github.com/rudderlabs/ai-sdk-go

Releases are automated with [release-please](https://github.com/googleapis/release-please). A Go module is published by its git tag: there is no registry upload step.

## One-time setup (repository admins)

1. **GitHub App for release-please.** `release-please.yml` uses a GitHub App token, as in `ai-sdk-node`, so release PRs get CI checks. `vars.RELEASE_APP_CLIENT_ID` and `secrets.RELEASE_PRIVATE_KEY` are available to this repository as organization-level values; the app must also be **installed on this repository** (contents and pull-requests write).
2. **Branch protection** on `main`: require pull requests and the CI checks. Pull requests are squash-merged, so the PR title becomes the commit release-please reads.

## How release-please works here

- On every push to `main`, release-please opens or updates a `chore: release x.y.z` PR that bumps `Version` in `version.go` (`x-release-please-version` marker) and the manifest, and updates `CHANGELOG.md`. Before 1.0, `feat` and breaking changes bump the minor version and `fix` the patch version.
- Merging that PR tags `vX.Y.Z` (no component prefix, as Go modules require) and publishes a GitHub release.
- The tag is the release: `go get github.com/rudderlabs/ai-sdk-go@vX.Y.Z` resolves it. The Go module proxy (`proxy.golang.org`) and `pkg.go.dev` pick the tag up on first request.
- Never move or delete a published tag: the Go checksum database records each version's hash, and a changed tag breaks every consumer. Fix forward with a new version, or add a `retract` directive to `go.mod` for a bad one.

## Modules

The repository holds several Go modules (see `go.work`): the core `github.com/rudderlabs/ai-sdk-go` (tags `vX.Y.Z`; it includes `ruddermcpgo`) and one per integration, `rudderopenai`, `rudderanthropic`, `ruddergenai`, `rudderbedrock` and `rudderotel` (tags `<dir>/vX.Y.Z`, as Go requires for a module in a subdirectory). release-please versions each from the commits under its directory. `internal/conformance` and `examples` are never tagged.

Each integration's `go.mod` requires a core release and replaces it with `../` for development; consumers ignore the replace. An integration that needs a new core API must require the core release that has it: release the core first, then bump the requirement (`go get github.com/rudderlabs/ai-sdk-go@vX.Y.Z` in that module) in the integration's release PR.

If release-please ever starts from the wrong commit, set `"bootstrap-sha"` in `release-please-config.json` to the commit of the last release and remove it after the next release PR.
