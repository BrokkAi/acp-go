package acp

import (
	"maps"

	"github.com/BrokkAi/acp-go/schema"
)

// SessionNoticesClientCapabilities advertises that the client can present
// live notice session updates to the user (session.notices: {}). Combine it
// with other session capabilities using SessionClientCapabilities.
func SessionNoticesClientCapabilities() *schema.ClientSessionCapabilities {
	return &schema.ClientSessionCapabilities{Notices: &schema.NoticeCapabilities{}}
}

// SessionCompactionClientCapabilities advertises the complete compaction
// contract (session.compaction: {}): compaction_update and
// compaction_summary_chunk, live and in replay. Combine it with other session
// capabilities using SessionClientCapabilities.
func SessionCompactionClientCapabilities() *schema.ClientSessionCapabilities {
	return &schema.ClientSessionCapabilities{Compaction: &schema.CompactionCapabilities{}}
}

// SessionClientCapabilities combines session capabilities, such as those
// from ConfigOptionsClientCapabilities, SessionNoticesClientCapabilities, and
// SessionCompactionClientCapabilities, into one value for
// Capabilities.Session. A field set by a later part replaces the same field
// from an earlier part, and _meta keys are merged the same way. Nil parts are
// skipped; the parts are not modified.
func SessionClientCapabilities(parts ...*schema.ClientSessionCapabilities) *schema.ClientSessionCapabilities {
	combined := schema.ClientSessionCapabilities{}
	for _, part := range parts {
		if part == nil {
			continue
		}
		if part.Meta != nil {
			if combined.Meta == nil {
				combined.Meta = schema.Meta{}
			}
			maps.Copy(combined.Meta, part.Meta)
		}
		if part.Compaction != nil {
			combined.Compaction = part.Compaction
		}
		if part.ConfigOptions != nil {
			combined.ConfigOptions = part.ConfigOptions
		}
		if part.Notices != nil {
			combined.Notices = part.Notices
		}
	}
	return &combined
}

// RequireSessionUpdateCapability reports whether an agent may send the
// session update to a client with the given capabilities, live or in
// session/load replay. In ACP v1 the agent must not send notice unless the
// client advertised session.notices, compaction_update or
// compaction_summary_chunk unless it advertised session.compaction, nor a
// config_option_update that carries a type: "boolean" option unless it
// advertised session.configOptions.boolean. Other updates need no client
// capability.
//
// A refusal is an *RPCError with code -32601, the error the agent runtime
// returns for a host method the client did not advertise, so errors.As
// identifies it. The agent runtime checks every update its SessionUpdater
// sends, and every session/update its Client.Notify sends with acp.Update
// params, before writing it to the wire.
func RequireSessionUpdateCapability(capabilities Capabilities, update schema.SessionUpdate) error {
	session := capabilities.Session
	switch {
	case update.Notice != nil:
		if session == nil || session.Notices == nil {
			return sessionUpdateNotAdvertised("session.notices")
		}
	case update.CompactionUpdate != nil, update.CompactionSummaryChunk != nil:
		if session == nil || session.Compaction == nil {
			return sessionUpdateNotAdvertised("session.compaction")
		}
	case update.ConfigOptionUpdate != nil:
		if session != nil && session.ConfigOptions != nil && session.ConfigOptions.Boolean != nil {
			return nil
		}
		for _, option := range update.ConfigOptionUpdate.ConfigOptions {
			if option.Boolean != nil || option.Kind == schema.SessionConfigOptionKindBoolean {
				return sessionUpdateNotAdvertised("session.configOptions.boolean")
			}
		}
	}
	return nil
}

func sessionUpdateNotAdvertised(capability string) error {
	return &RPCError{Code: -32601, Message: "client did not advertise " + capability + " support"}
}
