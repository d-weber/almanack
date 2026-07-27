//go:build unix

package main

import "syscall"

// withRestrictiveUmask runs fn with the process umask at 0077, so that anything
// created inside it is private to the owner from the moment it exists.
//
// It is here because chmod is too late for a file somebody else has already opened.
// VACUUM INTO creates the snapshot itself, under whatever umask the process inherited
// — 0022 on a stock system — and writes a complete copy of the calendar into it over
// however long the copy takes. Tightening the mode afterwards does not revoke a
// descriptor another process opened during that window, so the only fix is for the
// file never to have been readable at all.
//
// The umask is process-global, which is safe only because takeBackup runs in the
// one-shot `backup` subcommand and nothing else in that process creates files
// concurrently. Do not lift this into the server without thinking about that again.
func withRestrictiveUmask(fn func() error) error {
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)
	return fn()
}
