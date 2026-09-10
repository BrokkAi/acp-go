package acp

import "errors"

// ACP-specific JSON-RPC error codes.
const (
	ErrorCodeAuthRequired     = -32000
	ErrorCodeResourceNotFound = -32002
)

func rpcErrorCode(err error) (int, bool) {
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return rpc.Code, true
	}
	return 0, false
}

func IsAuthRequired(err error) bool {
	code, ok := rpcErrorCode(err)
	return ok && code == ErrorCodeAuthRequired
}

func IsResourceNotFound(err error) bool {
	code, ok := rpcErrorCode(err)
	return ok && code == ErrorCodeResourceNotFound
}
