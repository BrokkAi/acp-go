package acp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/BrokkAi/acp-go/internal/rpcctl"
)

func init() {
	rpcctl.WithRequestGuard = func(guard rpcctl.RequestGuard) any {
		return ConnectOption(func(c *Connection) { c.guard = guard })
	}
	rpcctl.WithNotificationGuard = func(guard rpcctl.NotificationGuard) any {
		return ConnectOption(func(c *Connection) { c.notifyGuard = guard })
	}
	rpcctl.WithOutgoingGuard = func(guard rpcctl.OutgoingGuard) any {
		return ConnectOption(func(c *Connection) { c.outGuard = guard })
	}
	rpcctl.WithOutgoingResponseGuard = func(guard rpcctl.OutgoingResponseGuard) any {
		return ConnectOption(func(c *Connection) { c.outResponse = guard })
	}
	rpcctl.WithIncomingResponseGuard = func(guard rpcctl.IncomingResponseGuard) any {
		return ConnectOption(func(c *Connection) { c.inResponse = guard })
	}
	rpcctl.V1InitializeResult = checkV1Selection
	rpcctl.QueueNotification = func(connection any, ctx context.Context, method string, params any) (func(context.Context) error, error) {
		c, ok := connection.(*Connection)
		if !ok || c == nil {
			return nil, errors.New("acp: notification queue needs an *acp.Connection")
		}
		return c.queueNotification(ctx, method, params)
	}
}

// queueNotification queues a notification frame and returns without waiting
// for it to be written. Unlike Notify, cancelling ctx after the frame is
// queued neither unqueues it nor stops the connection.
func (c *Connection) queueNotification(ctx context.Context, method string, params any) (func(context.Context) error, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w, _, err := c.queue(ctx, c.buildNotification(method, encoded))
	if c.droppedNotification(method, err) {
		return func(context.Context) error { return nil }, nil
	}
	if err != nil {
		return nil, err
	}
	return func(waitCtx context.Context) error {
		select {
		case err := <-w.sent:
			return err
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-c.ctx.Done():
			select {
			case err := <-w.sent:
				return err
			default:
				return c.Err()
			}
		}
	}, nil
}
