//go:build unix

package sampledconn

import (
	"errors"
	"io"
	"syscall"
)

func OSPeekConn(conn syscall.Conn) (PeekedBytes, error) {
	s := PeekedBytes{}

	rawConn, err := conn.SyscallConn()
	if err != nil {
		return s, err
	}

	var readErr error
	var n int
	err = rawConn.Read(func(fd uintptr) bool {
		n, _, readErr = syscall.Recvfrom(int(fd), s[:], syscall.MSG_PEEK|syscall.MSG_WAITALL)
		return !errors.Is(readErr, syscall.EAGAIN)
	})
	if err != nil {
		return s, err
	}
	if readErr != nil {
		return s, readErr
	}
	if n < peekSize {
		return s, io.ErrUnexpectedEOF
	}

	return s, nil
}
