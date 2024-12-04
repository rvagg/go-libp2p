//go:build windows

package sampledconn

import (
	"errors"
	"io"
	"syscall"

	"golang.org/x/sys/windows"
)

func OSPeekConn(conn syscall.Conn) (PeekedBytes, error) {
	s := PeekedBytes{}

	rawConn, err := conn.SyscallConn()
	if err != nil {
		return s, err
	}

	var readErr error
	var n uint32
	err = rawConn.Read(func(fd uintptr) bool {
		flags := uint32(windows.MSG_PEEK | windows.MSG_WAITALL)
		wsabuf := windows.WSABuf{
			Len: uint32(len(s)),
			Buf: &s[0],
		}

		readErr = windows.WSARecv(windows.Handle(fd), &wsabuf, 1, &n, &flags, nil, nil)
		return !errors.Is(readErr, windows.WSAEWOULDBLOCK)
	})
	if err != nil {
		return s, err
	}
	if readErr != nil {
		return s, readErr
	}
	if n < peekSize {
		return s, io.EOF
	}

	return s, nil
}
