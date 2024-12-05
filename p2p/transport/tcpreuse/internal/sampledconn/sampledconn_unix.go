//go:build unix

package sampledconn

import (
	"errors"
	"io"
	"syscall"

	"golang.org/x/sys/unix"
)

func isClosed(rawConn syscall.RawConn) bool {
	var isClosed bool
	_ = rawConn.Control(func(fd uintptr) {
		// Create pollfd struct for the file descriptor
		pollFd := []unix.PollFd{
			{
				Fd:      int32(fd),
				Events:  pollFlags,
				Revents: 0,
			},
		}

		// Call poll with a timeout of 0 for non-blocking behavior
		n, err := unix.Poll(pollFd, 0)
		if err != nil || (n > 0 && (pollFd[0].Revents != 0)) {
			isClosed = true // Error or hang-up event detected
		}
	})

	return isClosed
}

func OSPeekConn(conn syscall.Conn) (PeekedBytes, error) {
	s := PeekedBytes{}

	rawConn, err := conn.SyscallConn()
	if err != nil {
		return s, err
	}

	var readErr error
	var n int
	err = rawConn.Read(func(fd uintptr) bool {
		n, _, readErr = syscall.Recvfrom(int(fd), s[:], syscall.MSG_PEEK)
		if errors.Is(readErr, syscall.EAGAIN) {
			// We always retry if we get an EAGAIN err
			return false
		}

		if n >= peekSize {
			// We're done!
			return true
		}

		if n == 0 && readErr == nil {
			// Connection closed.
			return true
		}

		// We have peeked some bytes, we need to check explicitly if the connection
		// is already closed. If it is, we will not unblock from our pollWait.
		if isClosed(rawConn) {
			return true
		}
		return false
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
