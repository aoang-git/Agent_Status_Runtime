//go:build !windows

package monitor

import "os"

func ProtectDirectory(path string) error { return os.Chmod(path, 0700) }
