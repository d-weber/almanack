//go:build !unix

package main

// withRestrictiveUmask has nothing to do where there is no umask. The chmod that
// follows the snapshot still runs, and every platform this project releases for is
// covered by the unix build above; this exists so the package still compiles
// elsewhere rather than to make a promise it cannot keep.
func withRestrictiveUmask(fn func() error) error { return fn() }
