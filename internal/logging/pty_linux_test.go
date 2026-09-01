//go:build linux

package logging_test

import (
	"os"
	"unsafe"
)

const ioctlPtsname = 0x80045430 // TIOCGPTN

func grantpt(_ *os.File) error { return nil }

func unlockpt(f *os.File) error {
	var unlock int32

	return ioctl(f, 0x40045431, uintptr(unsafe.Pointer(&unlock))) // TIOCSPTLCK
}
