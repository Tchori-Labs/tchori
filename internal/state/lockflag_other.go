//go:build !unix

package state

// lockGuardFlags is zero where open-time no-follow and nonblocking guards are
// unavailable; post-acquisition descriptor verification detects raced entries.
const lockGuardFlags = 0
