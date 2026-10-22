//go:build unix

package main

import "syscall"

// restrictUmask makes every file the server creates private to its user:
// data/ holds the database (plugin logins, client secrets), the config with
// the master secret and the downloads.
func restrictUmask() { syscall.Umask(0o077) }
