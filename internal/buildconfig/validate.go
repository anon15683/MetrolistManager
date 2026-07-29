package buildconfig

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/MetrolistGroup/MetrolistManager/internal/version"
)

var repositoryPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type configuration struct {
	targetVersion    string
	releaseTag       string
	desktopAssetName string
	repositoryOwner  string
	repositoryName   string
}

func Validate() error {
	return validate(configuration{
		targetVersion:    TargetVersion,
		releaseTag:       ReleaseTag,
		desktopAssetName: DesktopAssetName,
		repositoryOwner:  RepositoryOwner,
		repositoryName:   RepositoryName,
	})
}

func validate(config configuration) error {
	if config.targetVersion == "0.0.0-dev" {
		return errors.New("development build is not bound to a Metrolist release")
	}
	if err := version.Validate(config.targetVersion); err != nil {
		return fmt.Errorf("invalid target version %q", config.targetVersion)
	}
	if strings.Contains(config.targetVersion, "-") {
		return errors.New("GitHub releases require a stable target version")
	}
	if config.desktopAssetName != "Metrolist-Desktop-windows.zip" {
		return errors.New(`desktop asset must be "Metrolist-Desktop-windows.zip"`)
	}
	if config.releaseTag != "v"+config.targetVersion {
		return fmt.Errorf("release tag must be %q", "v"+config.targetVersion)
	}
	if !repositoryPartPattern.MatchString(config.repositoryOwner) || !repositoryPartPattern.MatchString(config.repositoryName) {
		return errors.New("GitHub repository is not configured")
	}
	return nil
}
