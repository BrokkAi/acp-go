# Contributing to acp-go

Contributions from people using AI tools are welcome. Everyone remains
responsible for the accuracy, safety, licensing, and relevance of their work.
Please follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Issues and pull requests

Search existing issues and pull requests before opening a new one. For bugs,
include the version or commit, operating system, reproduction steps, expected
behavior, and actual behavior. Redact secrets and private source code from logs
and transcripts. Report vulnerabilities privately as described in
[SECURITY.md](SECURITY.md).

Keep changes focused. Discuss substantial behavior or interface changes with
maintainers before implementing them. A pull request should explain the
problem, resulting behavior, validation performed, and any remaining limits.
Link related issues and update documentation when behavior changes.

## Development and validation

Install the Go version in `go.mod` and Python 3. Run from the repository root:

```sh
go test -race ./...
go vet ./...
python3 scripts/licenses.py
python3 -m unittest discover -s scripts -p '*_test.py'
```

Format Go changes with `gofmt`. Add focused tests for behavior changes; ordinary
documentation changes need a diff and link review. Tests should use temporary
repositories and simulated agents, without publishing releases or requiring
live credentials.

### Optional wire fuzzing

The default test suite runs the fuzz seed corpus. To run a mutation campaign
locally:

```sh
go test -run '^$' -fuzz FuzzConnectionFrame -fuzztime 2m .
```

Commit any minimized regression corpus beside the fuzz target. Do not commit
the generated build cache.

### Credential-backed integration tests

Integration tests are behind the `integration` build tag and skip when no agent
is configured. They are never run by the default test command. To run one:

```sh
ACP_INTEGRATION_AGENT='["npx","-y","@agentclientprotocol/codex-acp"]' \
ACP_INTEGRATION_AUTH_METHOD='api-key' \
ACP_INTEGRATION_REQUIRED_ENV='OPENAI_API_KEY' \
go test -tags integration -run TestRealAgentPrompt -v ./integration
```

The command must be encoded as a JSON argv array so paths and arguments do not
need shell quoting. Keep prompts small and non-destructive, use temporary
workspaces, and never enable auto-approval for third-party integration tests.

## Schema generation

The `schema` package is generated from the pinned ACP JSON Schema release
(`schema/schema.json`, `schema/meta.json`, `schema/VERSION`) and must stay in
sync with it. To track a new schema release:

```sh
go run ./cmd/acpgen -update <version>   # e.g. 1.21.0; downloads and pins
go run ./cmd/acpgen                     # regenerates schema/*_gen*.go
go test -race ./schema/
```

Commit the regenerated files together with the updated pin, and review the
diff like any API change. Regeneration is deterministic; a clean tree after
`go run ./cmd/acpgen` proves the checked-in files match the pin. The generator
fails loudly on schema constructs it cannot model rather than guessing.

## Licensing and dependencies

This project uses [Apache-2.0](LICENSE). By intentionally submitting a
contribution for inclusion, you submit it under the project's license unless
you explicitly state otherwise, as described in section 5. Submit only work
you have the right to share and preserve upstream attribution and notices.

Dependency versions, legal texts, generated tables, and bundled assets require
license review. Follow [licenses/README.md](licenses/README.md), update the
reviewed policy and notices together, and commit `go.mod` and `go.sum` when
dependencies change. Do not add local replacement directives to a release.

## Releases

Keep [CHANGELOG.md](CHANGELOG.md) current with every user-visible change.
Before tagging:

1. Run the full default validation command suite and schema regeneration check.
2. Rename `Unreleased` to the next Semver version and release date.
3. Confirm the dependency policy, notices, and third-party tables still pass.
4. Create an annotated tag (`git tag -a v0.2.0 -m "..."`), push the branch and
   tag, and verify module consumers can resolve the tag.

During `0.x`, breaking Go API changes require a minor version bump. Once the
public API stabilizes, follow normal Semver compatibility rules.
