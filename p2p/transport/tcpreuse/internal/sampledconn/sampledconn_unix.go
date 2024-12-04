//go:build unix

package sampledconn

import (
	"errors"
	"io"
	"syscall"

	"golang.org/x/sys/unix"
)

// updateBlocking updates the blocking mode of the file descriptor.
// It returns true if the blocking mode was changed, and false if it was already in the desired mode.
// If an error occurs, it returns the error.
func updateBlocking(fd uintptr, shouldBlock bool) (bool, error) {
	flags, err := unix.FcntlInt(fd, syscall.F_GETFL, 0)
	if err != nil {
		return false, err
	}
	if shouldBlock && flags&syscall.O_NONBLOCK != 0 {
		// Clear O_NONBLOCK flag
		flags &^= unix.O_NONBLOCK
		_, err = unix.FcntlInt(fd, unix.F_SETFL, flags)
		return true, err
	}
	if !shouldBlock && flags&syscall.O_NONBLOCK == 0 {
		// Set O_NONBLOCK flag
		flags |= unix.O_NONBLOCK
		_, err = unix.FcntlInt(fd, unix.F_SETFL, flags)
		return true, err
	}
	return false, nil
}

func OSPeekConn(conn syscall.Conn) (PeekedBytes, error) {
	s := PeekedBytes{}

	rawConn, err := conn.SyscallConn()
	if err != nil {
		return s, err
	}

	var updatedBlocking bool
	ctlErr := rawConn.Control(func(fd uintptr) {
		updatedBlocking, err = updateBlocking(fd, true)
	})
	if ctlErr != nil {
		return s, ctlErr
	}
	if err != nil {
		return s, err
	}
	if updatedBlocking {
		defer func() {
			_ = rawConn.Control(func(fd uintptr) {
				_, _ = updateBlocking(fd, false)
			})
		}()
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
