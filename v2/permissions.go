package v2

import (
	"context"
	"errors"
	"sync"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// CancellablePermissions wraps a Permissions host and cancels pending requests
// when ActiveWork is cancelled. Context cancellation becomes the protocol's
// cancelled outcome rather than an error response.
type CancellablePermissions struct {
	next Permissions

	mu       sync.Mutex
	sequence uint64
	cancels  map[SessionID][]pendingPermission
}

var _ Permissions = (*CancellablePermissions)(nil)

func NewCancellablePermissions(next Permissions) *CancellablePermissions {
	return &CancellablePermissions{
		next:    next,
		cancels: make(map[SessionID][]pendingPermission),
	}
}

func (h *CancellablePermissions) RequestPermission(ctx context.Context, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	generation := h.register(request.SessionID, cancel)
	defer h.unregister(request.SessionID, generation)

	response, err := h.next.RequestPermission(ctx, request)
	if errors.Is(err, context.Canceled) {
		return CancelPermission(), nil
	}
	return response, err
}

func (h *CancellablePermissions) CancelPermissionRequests(sessionID SessionID) int {
	h.mu.Lock()
	pending := h.cancels[sessionID]
	delete(h.cancels, sessionID)
	h.mu.Unlock()
	for _, item := range pending {
		item.cancel()
	}
	return len(pending)
}

type pendingPermission struct {
	cancel     context.CancelFunc
	generation uint64
}

func (h *CancellablePermissions) register(sessionID SessionID, cancel context.CancelFunc) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sequence++
	h.cancels[sessionID] = append(h.cancels[sessionID], pendingPermission{
		cancel: cancel, generation: h.sequence,
	})
	return h.sequence
}

func (h *CancellablePermissions) unregister(sessionID SessionID, generation uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pending := h.cancels[sessionID]
	for i, item := range pending {
		if item.generation == generation {
			h.cancels[sessionID] = append(pending[:i], pending[i+1:]...)
			break
		}
	}
	if len(h.cancels[sessionID]) == 0 {
		delete(h.cancels, sessionID)
	}
}
