//go:build darwin

package cli

import "syscall"

const ioctlGetTermios = syscall.TIOCGETA
