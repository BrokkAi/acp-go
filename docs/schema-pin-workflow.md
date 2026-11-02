# Schema pin-change workflow

`acp-go` pins two schema artifacts copied byte-for-byte from one reference Rust
release, and both move together. [CONTRIBUTING.md](../CONTRIBUTING.md) holds the
generator commands; this page is the review checklist that runs around them.

## Trigger

The reference Rust SDK releases `agent-client-protocol` with a different
`agent-client-protocol-schema = "=X.Y.Z"` pin, or that schema crate publishes a
new version. [ROADMAP.md](../ROADMAP.md) records the current baseline: Rust
`agent-client-protocol` 5.0.0 pins schema crate `=1.15.1` → ACP v1
`schema-v1.29.1`, draft v2 `schema-v2.0.0-alpha.13`, and their `-unstable`
artifacts.

The release assets the downloader fetches are the same files the schema crate
embeds; to double-check a pin, compare them with `git show
v<crate-version>:schema/v1/schema.json` (and `meta.json`, `schema.unstable.json`,
`meta.unstable.json`, and the `schema/v2/` set) in the
[schema repository](https://github.com/agentclientprotocol/agent-client-protocol).

Do not follow the ACP schema repository `main` on its own; move on a released
schema crate or when the Rust SDK moves its exact pin.

## Steps

1. **Pin and regenerate.** Update both artifacts and regenerate every generated
   package from [CONTRIBUTING.md](../CONTRIBUTING.md#schema-generation):

   ```sh
   go run ./cmd/acpgen -update <version>
   go run ./cmd/acpgen                      # regenerate schema/*_gen*.go
   go run ./cmd/acpgen -update <version> \
     -schema schema/v2/schema.json -meta schema/v2/meta.json \
     -out schema/v2 -package v2
   go run ./cmd/acpgen -schema schema/v2/schema.json \
     -meta schema/v2/meta.json -out schema/v2 -package v2
   go run ./cmd/acpgen -update <version> -unstable \
     -schema schema/unstable/schema.json -meta schema/unstable/meta.json \
     -out schema/unstable -package unstable
   go run ./cmd/acpgen -schema schema/unstable/schema.json \
     -meta schema/unstable/meta.json -out schema/unstable -package unstable
   go run ./cmd/acpgen -update <v2-version> -unstable \
     -schema schema/v2/unstable/schema.json -meta schema/v2/unstable/meta.json \
     -out schema/v2/unstable -package unstable
   go run ./cmd/acpgen -schema schema/v2/unstable/schema.json \
     -meta schema/v2/unstable/meta.json -out schema/v2/unstable -package unstable
   ```

2. **Review the generated API diff.** This is the gate, not a formality. Read
   the `schema/**/*_gen*.go` diff for:

   - added, removed, or renamed request/response/notification types and method
     constants;
   - changed discriminated-union variants and `type` tag values;
   - new or removed enum members, and whether unknown tags still decode;
   - required/optional field changes, including pointer and `Nullable`
     differences between v1 and v2;
   - new v2 upsert updates or patch fields. Patch fields of the defs in
     `patchDefs` (`cmd/acpgen/ir.go`) keep `null` distinct from omission, and
     their upserts get a generated `ApplyUpdate` that patches the top-level
     `_meta` with `ApplyMetaUpdate`. Add a new upsert to `patchDefs` (a field
     it cannot patch fails generation), and check that the Metadata Patches
     rule still lists the same updates. v1 otherwise treats `null` like
     omission; a v1 field that Rust models with `MaybeUndefined` (today
     `CompactionUpdate`'s `summary`, `error`, and `_meta`, and
     `SessionInfoUpdate`'s `title` and `updatedAt`, and in the unstable pin
     `SessionMessage`'s and `SubagentUpdate`'s optional fields) is listed by
     def and field
     in `v1PatchDefs`, which keeps `null` distinct as `Nullable` without the
     v2 metadata patch helpers. A listed field that is missing, required, or
     not nullable fails generation;
   - JSON field names, integer widths, and defaults;
   - `x-deserialize-*` hints (default-on-error, default-on-null, skip invalid
     items). `cmd/acpgen` honors default-on-error only on the defs in
     `lenientDefs` (`cmd/acpgen/ir.go`): the state-update snapshots,
     `SessionStatusResponse`, and `SessionInfo`. It decodes every other field
     strictly and never skips invalid items. That is a known divergence from
     Rust, tracked in #82: a malformed value fails the whole message instead
     of reading as not reported. Record newly affected fields on #82, and extend `lenientDefs`
     where a malformed field would break a protocol MUST;
   - new `x-side` values or `meta.json` method groups (such as 1.15's
     `proxyMethods`): generation fails on a method outside the groups
     `cmd/acpgen` models, so teach it the group and its side constant;
   - `parity_gen_test.go` and `golden_test.go` updates, and any new goldens.

   Preserve the guardrails: open-world enums and unknown tags must keep
   decoding, raw `_meta` payloads must round-trip, and nothing may convert v1
   and v2 implicitly.

3. **Record the API differences.** Summarize user-visible additions and
   breaking changes in [CHANGELOG.md](../CHANGELOG.md) in the same change, the
   way the release notes describe earlier schema bumps.

4. **Validate.** Run the default suite from `CONTRIBUTING.md`
   (`go test -race ./...`, `go vet ./...`, `python3 scripts/licenses.py`,
   `python3 -m unittest discover -s scripts -p '*_test.py'`) plus
   `go test -race ./schema/`. Regeneration is deterministic: re-running the
   generator must leave a clean tree.

5. **Commit together.** Land the artifact pins, the regenerated files, the
   changelog entry, and any API-diff notes in one reviewed change.
