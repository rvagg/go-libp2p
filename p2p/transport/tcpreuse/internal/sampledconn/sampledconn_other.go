//go:build !windows && !linux && !darwin

package sampledconn

import (
	"syscall"
)

func OSPeekConn(conn syscall.Conn) (PeekedBytes, error) {
	return PeekedBytes{}, errNotSupported
}
