package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// BatchCall is one request in an outgoing JSON-RPC batch.
type BatchCall struct {
	Method string
	Params any
	Result any
}

// CallBatch sends requests as one JSON-RPC array and waits for their matching
// responses. Responses may be returned individually or in a response batch.
func (c *Connection) CallBatch(ctx context.Context, calls []BatchCall) []error {
	if len(calls) == 0 {
		return nil
	}
	packets := make([]packet, len(calls))
	replies := make([]chan packet, len(calls))
	ids := make([]string, len(calls))
	c.mu.Lock()
	for i := range calls {
		params, err := json.Marshal(calls[i].Params)
		if err != nil {
			c.mu.Unlock()
			result := make([]error, len(calls))
			for j := range result {
				if j == i {
					result[j] = err
				} else {
					result[j] = fmt.Errorf("batch was not sent: %w", err)
				}
			}
			return result
		}
		c.sequence++
		id := strconv.FormatUint(c.sequence, 10)
		ids[i] = id
		replies[i] = make(chan packet, 1)
		c.pending[id] = replies[i]
		packets[i] = packet{
			Version: "2.0", ID: json.RawMessage(id), Method: calls[i].Method, Params: params,
		}
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		for _, id := range ids {
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}()

	if err := c.sendJSON(ctx, packets); err != nil {
		result := make([]error, len(calls))
		for i := range result {
			select {
			case p := <-replies[i]:
				result[i] = decodeReply(p, calls[i].Result)
			default:
				result[i] = err
			}
		}
		return result
	}

	result := make([]error, len(calls))
	for i := range calls {
		select {
		case p := <-replies[i]:
			result[i] = decodeReply(p, calls[i].Result)
		case <-ctx.Done():
			result[i] = ctx.Err()
		case <-c.ctx.Done():
			select {
			case p := <-replies[i]:
				result[i] = decodeReply(p, calls[i].Result)
			default:
				if result[i] == nil {
					result[i] = c.Err()
				}
			}
		}
	}
	return result
}

type batchEntry struct {
	packet packet
	raw    json.RawMessage
	valid  bool
	// responseOnly marks an invalid object that looks like a response. Rust
	// ignores malformed response-shaped entries rather than answering them.
	responseOnly bool
}

func (e batchEntry) requestShaped() bool {
	return e.valid || !e.responseOnly
}

func (c *Connection) startBatch(raw json.RawMessage) {
	entries, err := parseBatch(raw)
	if err != nil {
		_ = c.sendJSON(c.ctx, invalidBatchResponse())
		return
	}
	c.tasks.Add(1)
	go func() {
		defer c.tasks.Done()
		responses, stop := c.processBatch(entries)
		if len(responses) > 0 {
			if err := c.sendJSON(c.ctx, responses); err != nil && stop == nil {
				stop = err
			}
		}
		if stop != nil {
			c.stop(stop)
		}
	}()
}

func parseBatch(raw json.RawMessage) ([]batchEntry, error) {
	var rawEntries []json.RawMessage
	if err := json.Unmarshal(raw, &rawEntries); err != nil {
		return nil, err
	}
	if len(rawEntries) == 0 {
		return nil, errors.New("empty JSON-RPC batch")
	}
	entries := make([]batchEntry, 0, len(rawEntries))
	for _, rawEntry := range rawEntries {
		entry := batchEntry{raw: rawEntry}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(rawEntry, &fields)
		_, hasMethod := fields["method"]
		_, hasResult := fields["result"]
		_, hasError := fields["error"]
		entry.responseOnly = !hasMethod && (hasResult || hasError)
		if err := json.Unmarshal(rawEntry, &entry.packet); err == nil && validPacket(entry.packet) {
			entry.valid = true
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func validPacket(p packet) bool {
	if p.Version != "2.0" {
		return false
	}
	if p.Method != "" {
		return len(p.ID) == 0 || (string(p.ID) != "null" && string(p.ID) != "")
	}
	return len(p.ID) > 0 && string(p.ID) != "null" && (p.Result == nil) != (p.Error == nil)
}

func (c *Connection) processBatch(entries []batchEntry) ([]packet, error) {
	type responseSlot struct {
		packet  packet
		pending <-chan packet
	}
	slots := make([]responseSlot, 0, len(entries))
	var initializeError *RPCError
	firstRequest := true
	for _, entry := range entries {
		switch {
		case entry.valid && entry.packet.Method == "":
			c.routeResponse(entry.packet)
		case entry.valid && len(entry.packet.ID) == 0:
			if entry.packet.Method == "$/cancel_request" {
				var cancelRequest struct {
					ID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(entry.packet.Params, &cancelRequest) == nil {
					c.cancelInbound(string(cancelRequest.ID))
				}
				continue
			}
			if c.onNotify != nil {
				if err := c.onNotify(entry.packet.Method, entry.packet.Params); err != nil {
					return nil, err
				}
			}
		case entry.valid:
			isFirstRequest := firstRequest
			firstRequest = false
			if isFirstRequest && entry.packet.Method == "initialize" {
				response := c.executeRequest(c.ctx, entry.packet)
				slots = append(slots, responseSlot{packet: response})
				if response.Error != nil {
					initializeError = response.Error
				}
				continue
			}
			if initializeError != nil {
				slots = append(slots, responseSlot{packet: packet{
					Version: "2.0", ID: entry.packet.ID, Error: initializeError,
				}})
				continue
			}
			slots = append(slots, responseSlot{pending: c.startBatchRequest(entry.packet)})
		case !entry.responseOnly:
			firstRequest = false
			slots = append(slots, responseSlot{packet: invalidBatchResponse()})
		}
	}
	responses := make([]packet, len(slots))
	for i, slot := range slots {
		if slot.pending == nil {
			responses[i] = slot.packet
		} else {
			responses[i] = <-slot.pending
		}
	}
	return responses, nil
}

func (c *Connection) startBatchRequest(p packet) <-chan packet {
	result := make(chan packet, 1)
	if err := c.reserveWorker(); err != nil {
		result <- packet{Version: "2.0", ID: p.ID, Error: &RPCError{Code: -32603, Message: "too many simultaneous ACP requests"}}
		return result
	}
	ctx, cancel := context.WithCancel(c.ctx)
	generation := c.registerInbound(string(p.ID), cancel)
	go func() {
		defer c.releaseWorker()
		response := c.executeRequest(ctx, p)
		cancel()
		c.unregisterInbound(string(p.ID), generation)
		result <- response
	}()
	return result
}

func (c *Connection) routeResponse(p packet) bool {
	c.mu.Lock()
	ch := c.pending[string(p.ID)]
	c.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- p:
		return true
	default:
		c.stop(errors.New("duplicate ACP response"))
		return false
	}
}

func invalidBatchResponse() packet {
	return packet{
		Version: "2.0",
		ID:      json.RawMessage("null"),
		Error:   &RPCError{Code: -32600, Message: "Invalid Request"},
	}
}
