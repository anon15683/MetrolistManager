//go:build !windows

package installer

import (
	"errors"
	"os"
)

func removeLauncherBeforeReplacement(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
