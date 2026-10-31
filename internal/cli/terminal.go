package cli

import (
	"os"
	"syscall"
	"unsafe"
)

func foregroundGroup() (int32, bool) {
	var pgrp int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&pgrp)))
	return pgrp, errno == 0
}

// holdsTerminal reports whether stdin is a terminal this process controls.
// Only then can the child be given the foreground: a child in a new group
// that stays in the background stops as soon as it reads the terminal.
func holdsTerminal() bool {
	pgrp, ok := foregroundGroup()
	return ok && int(pgrp) == syscall.Getpgrp()
}

func restoreTerminal() {
	pgrp := int32(syscall.Getpgrp())
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TIOCSPGRP, uintptr(unsafe.Pointer(&pgrp)))
}
