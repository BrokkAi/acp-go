package negotiate

import (
	"fmt"

	"github.com/BrokkAi/acp-go/internal/serdejson"
)

// implementationError reports why the info member of v2 initialize params,
// read as serde_json reads them, does not deserialize as the reference
// schema's v2 Implementation, in serde's words, or "" when it does. It is the
// only member of a v2 InitializeRequest that can fail: protocolVersion is
// checked first, and capabilities, _meta, and info's title and _meta default
// on error.
func implementationError(params *serdejson.Value) string {
	info, ok := params.Get("info")
	if !ok {
		return "missing field `info`"
	}
	switch info.Kind {
	case serdejson.Object:
		seen := map[string]bool{}
		for _, member := range info.Members {
			if member.Key != "name" && member.Key != "version" {
				continue
			}
			seen[member.Key] = true
			if member.Value.Kind != serdejson.String {
				return serdejson.InvalidType(member.Value, "a string")
			}
		}
		for _, key := range []string{"name", "version"} {
			if !seen[key] {
				return fmt.Sprintf("missing field `%s`", key)
			}
		}
		return ""
	case serdejson.Array:
		// A struct also deserializes from a sequence of its fields in order:
		// name, title, version, _meta. title and _meta default when absent.
		const length = "struct Implementation with 4 elements"
		if len(info.Items) == 0 {
			return "invalid length 0, expected " + length
		}
		if info.Items[0].Kind != serdejson.String {
			return serdejson.InvalidType(info.Items[0], "a string")
		}
		if len(info.Items) < 3 {
			return "invalid length 2, expected " + length
		}
		if info.Items[2].Kind != serdejson.String {
			return serdejson.InvalidType(info.Items[2], "a string")
		}
		if len(info.Items) > 4 {
			return fmt.Sprintf("invalid length %d, expected fewer elements in array", len(info.Items))
		}
		return ""
	default:
		return serdejson.InvalidType(info, "struct Implementation")
	}
}
