package logging_test

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openPTY opens a pseudo-terminal pair, so terminal detection can be tested
// for real rather than mocked. wsaw targets Linux and macOS only, so posix
// openpt is available everywhere it runs.
func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening /dev/ptmx: %w", err)
	}

	if err := grantpt(m); err != nil {
		_ = m.Close()

		return nil, nil, err
	}

	if err := unlockpt(m); err != nil {
		_ = m.Close()

		return nil, nil, err
	}

	name, err := ptsname(m)
	if err != nil {
		_ = m.Close()

		return nil, nil, err
	}

	s, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = m.Close()

		return nil, nil, fmt.Errorf("opening %s: %w", name, err)
	}

	return m, s, nil
}

func ioctl(f *os.File, req uint, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(req), arg)
	if errno != 0 {
		return fmt.Errorf("ioctl: %w", errno)
	}

	return nil
}

func ptsname(f *os.File) (string, error) {
	var buf [128]byte

	if err := ioctl(f, ioctlPtsname, uintptr(unsafe.Pointer(&buf[0]))); err != nil {
		return "", err
	}

	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), nil
		}
	}

	return string(buf[:]), nil
}
