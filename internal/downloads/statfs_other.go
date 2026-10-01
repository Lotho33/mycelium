//go:build !linux

package downloads

// diskFree is unknown off Linux: only the quota applies there.
func diskFree(string) (int64, bool) { return 0, false }
