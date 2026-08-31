//go:build unix

package state

import "syscall"

// lockGuardFlags refuses symlinks at open time and makes filesystem FIFO opens
// raced after preflight return promptly on the shipped POSIX targets.
const lockGuardFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
