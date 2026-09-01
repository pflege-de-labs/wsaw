//go:build darwin

package logging_test

import "os"

const ioctlPtsname = 0x40807453 // TIOCPTYGNAME

func grantpt(f *os.File) error  { return ioctl(f, 0x20007454, 0) } // TIOCPTYGRANT
func unlockpt(f *os.File) error { return ioctl(f, 0x20007452, 0) } // TIOCPTYUNLK
