//go:build windows

package skills

import "os"

// csm refuses to run on Windows; the library only has to build there.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) {}
