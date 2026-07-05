//go:build windows

package upgrade

import (
	"syscall"
	"unsafe"
)

const (
	movefileReplaceExisting  = 0x1
	movefileDelayUntilReboot = 0x4
)

func scheduleReplace(source string, target string) error {
	sourcePtr, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	moveFileExW := kernel32.NewProc("MoveFileExW")
	ret, _, callErr := moveFileExW.Call(
		uintptr(unsafe.Pointer(sourcePtr)), // #nosec G103 -- documented Windows syscall interop: UTF-16 pointer is converted only for this immediate Call.
		uintptr(unsafe.Pointer(targetPtr)), // #nosec G103 -- documented Windows syscall interop: UTF-16 pointer is converted only for this immediate Call.
		uintptr(movefileReplaceExisting|movefileDelayUntilReboot),
	)
	if ret == 0 {
		return callErr
	}
	return nil
}
