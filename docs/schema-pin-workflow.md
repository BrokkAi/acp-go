# Schema pin-change workflow

`acp-go` pins two schema artifacts copied byte-for-byte from one reference Rust
release, and both move together. [CONTRIBUTING.md](../CONTRIBUTING.md) holds the
generator commands; this page is the review checklist that runs around them.

## Trigger

The reference Rust SDK releases `agent-client-protocol` with a different
`agent-client-protocol-schema = "=X.Y.Z"` pin, or that schema crate publishes a
new version. [ROADMAP.md](../ROADMAP.md) records the current baseline (schema
crate 1.9.1 → ACP v1 `schema-v1.23.0`, draft v2
`schema-v2.0.0-alpha.5`).

Do not follow the ACP schema repository on its own; move only when the Rust SDK
moves its exact pin.

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
   go run ./cmd/acpgen -schema schema/unstable/schema.json \
     -meta schema/unstable/meta.json -out schema/unstable -package unstable
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
   - JSON field names, integer widths, and defaults;
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
