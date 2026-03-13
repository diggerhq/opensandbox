package qemu

import "os"

// newFileFromFD wraps a raw file descriptor in an *os.File.
func newFileFromFD(fd int) *os.File {
	return os.NewFile(uintptr(fd), "vsock")
}
