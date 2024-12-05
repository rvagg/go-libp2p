//go:build linux

package sampledconn

import (
	"golang.org/x/sys/unix"
)

var pollFlags = int16(unix.POLLERR | unix.POLLHUP | unix.POLLRDHUP)
