//go:build windows

package installer

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func removeLauncherBeforeReplacement(path string) error {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ, windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	}
	removeErr := os.Remove(path)
	closeErr := windows.CloseHandle(handle)
	return errors.Join(removeErr, closeErr)
}

func replaceFile(source, destination string) error {
	sourcePointer, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPointer, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	err = windows.MoveFileEx(sourcePointer, destinationPointer, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return windows.MoveFileEx(sourcePointer, destinationPointer, windows.MOVEFILE_WRITE_THROUGH)
	}
	return err
}
