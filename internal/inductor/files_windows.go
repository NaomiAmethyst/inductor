// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func fileIdentity(path string, _ os.FileInfo) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return "", false
	}
	index := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return fmt.Sprintf("%d:%d", info.VolumeSerialNumber, index), true
}

func lockFile(f *os.File) error {
	// Lock the entire range, including future appends, and wait for other writers.
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK,
		0, 0xffffffff, 0xffffffff, &windows.Overlapped{})
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0,
		0xffffffff, 0xffffffff, &windows.Overlapped{})
}
