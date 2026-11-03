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
   python3 scripts/capture_hint_fixtures.py   # recapture the Rust decode and encode outcomes
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
   - `x-deserialize-*` hints and required fields. The generated decoders
     follow them on every field (see [Decode rules](#decode-rules)), so the
     diff shows changed decoding wherever a hint or a `required` list
     changed. A decode rule the schema cannot express belongs in the tables
     in `cmd/acpgen/ir.go` (see [Decode rules](#decode-rules)), and a new
     hint the generator does not know is a generator change. Generation
     fails when the method payloads without required fields stop matching
     `defaultOnNullPayloads`, the Rust crate's `default_on_null!` inventory;
   - new `x-side` values or `meta.json` method groups (such as 1.15's
     `proxyMethods`): generation fails on a method outside the groups
     `cmd/acpgen` models, so teach it the group and its side constant;
   - `parity_gen_test.go` and `golden_test.go` updates, and any new goldens;
   - `hints_gen_test.go` and `testdata/hints_rust.json`: recapture the Rust
     outcomes (see [Decode rules](#decode-rules) and
     [Encode rules](#encode-rules)) and review the fixture diff next to the
     generated code. A new union a Rust struct flattens beside its own fields
     fails generation until `flattenAfter` says where Rust declares it.

   Preserve the guardrails: open-world enums and unknown tags must keep
   decoding, raw `_meta` payloads must round-trip, and nothing may convert v1
   and v2 implicitly.

3. **Record the API differences.** Summarize user-visible additions and
   breaking changes in [CHANGELOG.md](../CHANGELOG.md) in the same change, the
   way the release notes describe earlier schema bumps.

4. **Validate.** Run the default suite from `CONTRIBUTING.md`
   (`go test -race ./...`, `go vet ./...`, `python3 scripts/licenses.py`,
   `python3 -m unittest discover -s scripts -p '*_test.py'`) plus
   `go test -race ./schema/...`. Before that, recapture the Rust decode
   outcomes with `python3 scripts/capture_hint_fixtures.py` (it needs
   cargo). Regeneration is deterministic: re-running the generator must leave
   a clean tree, and so must re-running the capture.

5. **Commit together.** Land the artifact pins, the regenerated files, the
   recaptured fixtures, the changelog entry, and any API-diff notes in one
   reviewed change.

## Decode rules

The generated decoders accept and reject the same inputs as the pinned Rust
schema crate and produce the same values. The reference SDK parses a message
into a `serde_json::Value` and decodes the payload from it; the generated
decoders follow serde's derives and the schema's hints, which record each
field's serde adapters:

- `x-deserialize-default-on-error` (`serde_with::DefaultOnError`): a value
  that does not decode reads as the field's default. For an optional field
  that is "not reported", and `null` reads the same way. A `Nullable` patch
  field (`DefaultOnError<MaybeUndefined<_>>`) keeps an explicit `null` as
  null; only a malformed value reads as unset (Rust's
  `MaybeUndefined::Undefined`). A required list or map defaults to an empty
  one. A required field must still be present.
- `x-deserialize-skip-invalid-items` (`serde_with::VecSkipError`): list items
  that do not decode are dropped and the others kept. A value that is not a
  list fails, unless default-on-error also applies.
- `x-deserialize-default-on-null` (`serde_with::DefaultOnNull`): `null` reads
  as the default.

Everything else decodes as serde does:

- A required field must be present. `null` is accepted only where the schema
  admits it (an `Option` in Rust), for list items and map values too. Wire
  keys match exactly, and a duplicate key keeps its last value, as in
  `serde_json::Value`.
- Enums read as serde reads unit variants: a string, or the one-key map form
  `{"variant": null}` (a non-null value or more keys fail). A closed enum
  (one without a catch-all member: most v1 enums, and `ElicitationSchemaType`
  in both versions) rejects a value it does not name; v1 `ToolKind` reads one
  as `other` (`#[serde(other)]`). An open enum keeps any string, and the map
  form only for a variant it names. `null` is never an enum value.
- A union of primitives (an untagged Rust enum such as
  `SessionConfigSelectOptions`, `ElicitationContentValue`, or `RequestId`)
  accepts only a value one of its arms decodes, and holds the Go value of the
  first such arm: a `string`, an integer of the arm's width (`int64`), a
  `float64`, a `bool`, `nil` for `null`, a typed slice (such as
  `[]SessionConfigSelectOption`), or the `json.RawMessage` of an
  unconstrained arm.
- An `f64` reads as serde_json reads it. Built without its `float_roundtrip`
  feature, as the reference SDK is, serde_json does not always round to the
  nearest `float64` (`7.2e-300` reads as `7.199999999999999e-300`), and
  neither do the decoders.
- An optional raw JSON field that Rust holds as an `Option<Value>` reads
  `null` as `nil` (None); a patch field keeps it as `json.RawMessage("null")`.
- A tagged union routes on its tag; whatever its tagged variants do not decode
  (a missing, invalid, or unknown tag, or a payload that does not decode) goes
  to its untagged variants in schema order. The open variant takes a string
  tag that names no variant, and only with the fields (and scope) its schema
  arm requires; a tag-less variant takes any value its payload decodes.
- A struct also decodes from a JSON array, positionally in Rust field
  declaration order (the schema's property order): a missing element leaves
  an optional field unset and fails a required one, and extra elements fail.
  A tagged union decodes from an array whose first element is the tag and
  whose rest is the payload. A Rust struct with a flattened field decodes only
  from an object: unions with shared fields, structs carrying a flattened
  union (`IdleStateUpdate`), and the plain structs in `mapOnlyStructs` (v1
  `ToolCallUpdate` and `WaitForTerminalExitResponse`, and `McpError`); v2
  `ToolCallUpdate` flattens nothing and reads from an array.
- Where serde decodes from buffered content (inside a default-on-error field,
  a skip-invalid list item, a tagged union's payload, a union with untagged
  variants or shared fields, or the flattened `IdleStateUpdate.stopReason`),
  a non-negative integer tag selects the tagged variant at that index, as
  serde's variant identifiers do. Elsewhere, such as a top-level v1
  `ContentBlock` or a plain `SessionNotification.update`, a numeric tag fails.
  The decoders carry this context as a `buffered` flag.
- A flattened union is a Rust flattened `Option`: a stop reason that does not
  decode reads as not reported.
- A struct without required fields decodes from top-level `null` as its zero
  value. That matches Rust on the wire: the generator requires the method
  payloads without required fields to be exactly the crate's
  `default_on_null!` payloads (`defaultOnNullPayloads`), the only ones Rust
  reads from `null`; other structs never arrive as a top-level `null`.

The rules the schema cannot express are tables in `cmd/acpgen/ir.go`, each
checked at generation:

- `nonZeroFields`: integers Rust decodes as `NonZero` (v2
  `ToolCallLocation.line`).
- `flattenReservedKeys`: keys Rust keeps out of a custom stop reason.
- `enumFallbacks`: closed enums with a `#[serde(other)]` member.
- `mapOnlyStructs`: plain structs with a `#[serde(flatten)]` field in Rust,
  per protocol version; an entry that is not a plain struct fails generation.
- `stringTagUnions`: unions whose hand-written Rust `Deserialize` requires a
  string tag (v2 `SetSessionConfigOptionRequest`), with the errors it words
  itself.
- `defaultOnNullPayloads`: the `default_on_null!` inventory.
- `pathFields`: v1 string fields Rust decodes as a `PathBuf`, whose errors
  expect a "path string". v2 names the type `AbsolutePath`, so it needs no
  table.
- `flattenedEnums`: the Rust name of the enum each union with shared fields
  and untagged variants flattens, which its errors name.
- `flattenAfter`: where each Rust struct declares the union it flattens
  beside its own fields, which sets where encoding writes the union's
  members.
- `rawPatchFields` (`cmd/acpgen/encode.go`): raw JSON fields outside the
  patch tables that Rust holds as `MaybeUndefined<Value>` (`McpError.data`).

Each package's generated `hints_gen_test.go` derives decode cases from the
schema: valid, missing, `null`, and malformed values for every hinted field;
missing and `null` for every required field; `null` items and values for
every list and map; an unknown value and the map forms for every enum field,
an object for every union-of-primitives field, and an integer tag for every
tagged-union field;
the array form of every struct (short, long, and empty) and tagged union;
and missing, `null`, non-string, empty, custom, integer, and bare known and
custom tags for every tagged union. `scripts/capture_hint_fixtures.py` runs
them through `scripts/hintprobe`, a probe built on the pinned crate that
decodes through `serde_json::Value` as the SDK does, and writes the outcomes
to `testdata/hints_rust.json`. `TestDeserializeHints` requires the Go
decoders to accept and reject the same inputs and, for accepted ones, to
produce the same value: it decodes the Rust re-encoding with the Go type and
compares the two Go values field by field. A field one side leaves unset
equals the other only when the other holds that field's default (the
schema's, or one Rust omits when encoding, `rustOmittedDefaults`), an empty
list equals an absent one, and a `Nullable` null equals omission except on
patch fields, at any depth. A stale or missing fixture fails the test.

`TestDeserializeHints` also requires each rejected input to fail with
serde's exact error text, which the agent runtimes and client hosts send to
peers in `-32602` answers (see below). The decoders word errors as serde's
derives do: one error, without a field path, for the first value that does
not decode in wire order (each key at its first position, with its last
value, as in `serde_json::Value`), then ``missing field `x` `` for the first
required field absent in Rust declaration order. A value of the wrong type
is ``invalid type: integer `5`, expected a string`` (or `u16`, `path
string`, `struct EnvVariable`, `a sequence`, and so on), an integer out of
range ``invalid value: integer `-1`, expected u32``, an unknown enum value
or tag ``unknown variant `x`, expected one of ...``, and a union Rust reads
as untagged `data did not match any variant of untagged enum X`. Errors
never name Go types.

The generated cases find no remaining accept/reject, value, or error text
differences.

## Encode rules

The generated types encode exactly as the pinned Rust schema crate serializes
the same value with `serde_json::to_string` (built with `preserve_order`, as
the reference SDK is). Every struct and union has a generated `MarshalJSON`;
the struct tags no longer drive encoding.

- Fields are written in Rust declaration order, the schema's property order,
  which is also the order the Go fields are declared in. A union a Rust
  struct flattens is written where the struct declares it (`flattenAfter`):
  an internally tagged union writes its tag first, then its payload's fields.
- An `Option` that is `None` is left out (`skip_serializing_none`), and so is
  a `null` in a `Nullable` field that is not a patch, since Rust reads it as
  `None`. A patch field (Rust `MaybeUndefined`) writes its explicit `null`.
- A field that is not an `Option` but has a default (`#[serde(default)]`) is
  always written: the v1 capability flags (`"loadSession":false`,
  `"fs":{"readTextFile":false,"writeTextFile":false}`, ...), v1
  `InitializeResponse.authMethods`, v2 `InitializeRequest.capabilities` and
  `InitializeResponse.capabilities`, and `ElicitationSchema.type` and
  `properties`. An unset Go field writes the
  schema's default. A required list or map writes `[]` or `{}` when nil.
- A list or map that is not an `Option` and has no default is left out when
  empty (`skip_serializing_if = "Vec::is_empty"`); an `Option` list or map
  is written when non-nil, empty included.
- A field Rust leaves out when it holds its default (`rustOmittedDefaults`:
  v1 `ToolCall.kind` `other` and `status` `pending`) is left out then.
- An `f64` is written as serde_json formats it, always with a decimal point
  or an exponent: `1.0`, `-0.0`, `0.1`, `1e+20`, `1e-7`, `5e-324`.
- A raw JSON value Rust holds as a `serde_json::Value` (an optional raw
  field, an open variant's preserved payload, a decoded `ErrorData`, a
  successor response) is re-encoded as serde_json re-encodes a `Value`:
  compact, members in order, a repeated key once at its first position with
  its last value, numbers and escapes in serde_json's form. An open variant of
  a tagged union is written as Rust's `Other` type writes it: the tag, the
  fields the type declares, then its other members sorted by key (a flattened
  `BTreeMap`), without the keys of the enclosing struct, and for an
  elicitation mode the keys of the scope that matched, without those of the
  other scope.
- The other members of a struct whose schema admits additional properties
  (`McpError`, whose Rust type flattens a `serde_json::Map`) are kept, in
  wire order, in its `Extra` field and written after its fields.
- A union of primitives writes each Go value as the Rust arm holding it.
- Strings are escaped as serde_json escapes them. `json.Marshal` further
  escapes `<`, `>`, `&`, U+2028, and U+2029 in any `MarshalJSON` output, as
  it does for every type.

`Meta` and the other untyped values (`any`) are decoded and written by
`encoding/json`, which sorts map keys and holds every number as a `float64`,
losing the integers above 2^53; Rust keeps the member order and the integer
and float forms of a `serde_json::Value`. Their strings and numbers are
written in serde_json's form.

`acp.Connection` writes params and results it encodes from a Go value with
`serdewire.EncodeTyped`, which only undoes `json.Marshal`'s escapes and
writes each float64 as serde_json writes it, without re-reading its digits
as serde_json reads wire text: the reference SDK writes a Rust value with
`serde_json::to_value` and `to_string`, which never parse a number it holds.
Params forwarded as a `json.RawMessage` are wire text and are read as
serde_json reads them (`serdewire.Encode`).

The encode cases in `hints_gen_test.go` decode, for every def, its minimal
and full documents, the full document of every union variant, every `f64`
field set to integral, negative-zero, large, small, subnormal, and inexact
values, every integer field set to `-0` (which serde_json reads as the f64
`-0.0` and an integer rejects), every optional list and map set empty, every
raw JSON field set to a value with a repeated key, floats, and escapes, every
arm of every union-of-primitives field, a custom elicitation mode carrying
both scopes, the other members of `McpError`, and a string with every escape
class, and capture
`serde_json::to_string` of the Rust value alongside the decode outcome.
`TestDeserializeHints` requires the frame writer's encoding
(`serdewire.MarshalTyped`) and the generated `MarshalJSON` of the decoded Go
value to be byte for byte that string, and `json.Marshal` too wherever it
holds none of the characters `json.Marshal` escapes. They find no remaining
differences.

## Params decoding in the dispatch layers

Every acp-go layer that dispatches a request to a typed handler (the v1 and
draft-v2 agent runtimes, `agent.HandleClientHost` and its parts, the v2
`HandleClientHost`, `acp.HandleElicitation`, `clienthost.Host`, the
unstable `Handle` routes, and the `mcp` and `v2/mcp` routers for
`mcp/message` envelopes and session setup params) decodes params as the
reference SDK 5.0.0 does:

- Absent params read as `null` for every message, as Rust's
  `params_from_transport` hands them over, so a message without params
  decodes, and fails, exactly like one with `"params": null`. A
  `default_on_null!` payload such as `session/list` decodes from either as
  its default; any other request fails with
  ``invalid type: null, expected struct X``.
- Params that do not decode answer `json_cast_params`' `-32602`:
  `{"code":-32602,"message":"Invalid params","data":{"error":<serde error>,"json":<params>,"phase":"deserialization"}}`,
  where `json` holds the params as `serde_json::Value` does (compact, a
  repeated key at its first position with its last value, numbers as
  serde_json writes them, `null` when absent).

A request no handler serves, including an extension method and an optional
method the agent does not implement or advertise, answers
`{"code":-32601,"message":"Method not found","data":"<method>"}`, the
reference SDK's `reject_unhandled` answer for both roles and protocol
versions; an unknown notification is ignored.

Decoding stays linear in the size of the params: the decoders merge repeated
keys of an object in one pass, as does the echo of the params.

`internal/castparams` builds that data and `internal/rpcparams` the errors;
each dispatch layer uses them rather than its own. The notification adapters
(`SessionUpdates`, `ElicitationComplete`, the session projections and
trackers, and the runtimes' `session/cancel`) read absent params as `null`
too.
