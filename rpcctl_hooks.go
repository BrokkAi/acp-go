package acp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/BrokkAi/acp-go/internal/rpcctl"
)

func init() {
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
	frame, err := json.Marshal(packet{Version: "2.0", Method: method, Params: encoded})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w := outgoing{data: append(frame, '\n'), sent: make(chan error, 1)}
	select {
	case c.writes <- w:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.Err()
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
