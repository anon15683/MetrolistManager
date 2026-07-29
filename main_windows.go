//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MetrolistGroup/MetrolistManager/internal/buildconfig"
	"github.com/MetrolistGroup/MetrolistManager/internal/githubrelease"
	"github.com/MetrolistGroup/MetrolistManager/internal/installer"
	"github.com/MetrolistGroup/MetrolistManager/internal/operation"
	"github.com/MetrolistGroup/MetrolistManager/internal/winplatform"
	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

type options struct {
	command         string
	silent          bool
	installRoot     string
	desktopShortcut bool
	desktopSet      bool
	launch          bool
	runningPID      uint32
	parentPID       uint32
	installationID  string
	force           bool
	allowDowngrade  bool
	purgeData       bool
	rootSet         bool
	installed       bool
}

var managerLog *os.File

func main() {
	setupLogging()
	options, err := parseOptions(os.Args[1:])
	if err != nil {
		fail(nil, options.silent, err)
	}
	if options.command == "uninstall-worker" {
		if err := runUninstallWorker(options); err != nil {
			fail(nil, options.silent, fmt.Errorf("uninstall failed: %w", err))
		}
		return
	}
	releaseLock, err := winplatform.AcquireManagerLock()
	if err != nil {
		fail(nil, options.silent, err)
	}
	defer releaseLock()
	if options.command == "uninstall" {
		if options.silent {
			if err := startUninstall(options.purgeData, options.force, true, nil); err != nil {
				fail(nil, true, err)
			}
			return
		}
		runUninstallUI(options)
		return
	}
	if err := buildconfig.Validate(); err != nil {
		fail(nil, options.silent, fmt.Errorf("invalid manager build configuration: %w", err))
	}

	installedRoot, installedVersion, installedErr := winplatform.LoadInstallation()
	if installedErr != nil && !errors.Is(installedErr, winplatform.ErrNotInstalled) {
		fail(nil, options.silent, installedErr)
	}
	installed := installedErr == nil
	options.installed = installed
	if installed {
		if options.rootSet && !samePath(options.installRoot, installedRoot) {
			fail(nil, options.silent, errors.New("Metrolist Desktop is already installed in a different directory"))
		}
		options.installRoot = installedRoot
		if marker, markerErr := installer.ReadMarker(installedRoot); markerErr == nil && !options.desktopSet {
			options.desktopShortcut = marker.DesktopShortcut
		}
		comparison, err := installer.CompareVersions(buildconfig.TargetVersion, installedVersion)
		if err != nil {
			fail(nil, options.silent, err)
		}
		if comparison < 0 && !options.allowDowngrade {
			fail(nil, options.silent, fmt.Errorf("version %s is already installed; this installer is for older version %s", installedVersion, buildconfig.TargetVersion))
		}
		if options.runningPID != 0 {
			exists, matches, processErr := winplatform.ValidateAppProcess(installedRoot, options.runningPID)
			if processErr != nil {
				fail(nil, options.silent, fmt.Errorf("validate Metrolist process: %w", processErr))
			}
			if exists && !matches {
				fail(nil, options.silent, errors.New("the supplied process ID does not belong to the installed Metrolist Desktop"))
			}
		}
	} else {
		if options.command == "update" || options.command == "repair" {
			fail(nil, options.silent, winplatform.ErrNotInstalled)
		}
		if options.installRoot == "" {
			options.installRoot, err = winplatform.DefaultInstallRoot()
			if err != nil {
				fail(nil, options.silent, err)
			}
		}
	}

	if options.silent {
		if err := runOperation(context.Background(), options, nil, nil); err != nil {
			fail(nil, true, err)
		}
		return
	}
	automatic := options.command == "update"
	runInstallUI(options, installedVersion, automatic)
}

func parseOptions(arguments []string) (parsed options, err error) {
	// flag.BoolVar assigns its default when the flag is defined, so parsed.silent is
	// reset before parsing and stays false whenever an invalid flag stops the parser
	// ahead of --silent. Restore the pre-scan result on every error path.
	silentRequested := requestsSilent(arguments)
	parsed = options{command: "install", launch: true, silent: silentRequested}
	defer func() {
		if err != nil && silentRequested {
			parsed.silent = true
		}
	}()
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		parsed.command = arguments[0]
		arguments = arguments[1:]
	}
	validCommands := map[string]bool{"install": true, "update": true, "repair": true, "uninstall": true, "uninstall-worker": true}
	if !validCommands[parsed.command] {
		return parsed, fmt.Errorf("unknown command %q", parsed.command)
	}
	flags := flag.NewFlagSet(parsed.command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&parsed.silent, "silent", false, "run without a window")
	flags.StringVar(&parsed.installRoot, "install-dir", "", "installation directory")
	flags.BoolVar(&parsed.desktopShortcut, "desktop-shortcut", false, "create a desktop shortcut")
	flags.BoolVar(&parsed.launch, "launch", true, "launch after installation")
	pid := flags.Uint64("pid", 0, "Metrolist process ID")
	flags.BoolVar(&parsed.force, "force", false, "terminate Metrolist after the shutdown timeout")
	flags.BoolVar(&parsed.allowDowngrade, "allow-downgrade", false, "allow installing an older version")
	flags.BoolVar(&parsed.purgeData, "purge-data", false, "remove Metrolist user data when uninstalling")
	parentPID := flags.Uint64("parent-pid", 0, "uninstaller parent process ID")
	flags.StringVar(&parsed.installationID, "installation-id", "", "installation identity")
	if parseErr := flags.Parse(arguments); parseErr != nil {
		return parsed, parseErr
	}
	visited := make(map[string]bool)
	flags.Visit(func(item *flag.Flag) {
		visited[item.Name] = true
		if item.Name == "install-dir" {
			parsed.rootSet = true
		}
		if item.Name == "desktop-shortcut" {
			parsed.desktopSet = true
		}
	})
	if *pid > uint64(^uint32(0)) || *parentPID > uint64(^uint32(0)) {
		return parsed, errors.New("process ID is out of range")
	}
	parsed.runningPID = uint32(*pid)
	parsed.parentPID = uint32(*parentPID)
	if flags.NArg() != 0 {
		return parsed, errors.New("unexpected positional arguments")
	}
	if validationErr := validateCommandFlags(parsed.command, visited); validationErr != nil {
		return parsed, validationErr
	}
	return parsed, nil
}

func validateCommandFlags(command string, visited map[string]bool) error {
	allowed := map[string]map[string]bool{
		"install": {
			"silent": true, "install-dir": true, "desktop-shortcut": true, "launch": true,
			"force": true, "allow-downgrade": true,
		},
		"update": {
			"silent": true, "pid": true, "desktop-shortcut": true, "launch": true,
			"force": true, "allow-downgrade": true,
		},
		"repair": {
			"silent": true, "desktop-shortcut": true, "launch": true,
			"force": true, "allow-downgrade": true,
		},
		"uninstall": {
			"silent": true, "purge-data": true, "force": true,
		},
		"uninstall-worker": {
			"silent": true, "install-dir": true, "parent-pid": true, "installation-id": true,
		},
	}
	for name := range visited {
		if !allowed[command][name] {
			return fmt.Errorf("--%s is not valid for %s", name, command)
		}
	}
	return nil
}

func requestsSilent(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--silent" || argument == "--silent=true" {
			return true
		}
	}
	return false
}

func runInstallUI(options options, installedVersion string, automatic bool) {
	installed := options.installed
	var window *walk.MainWindow
	var destination *walk.LineEdit
	var browse *walk.PushButton
	var desktop *walk.CheckBox
	var launch *walk.CheckBox
	var status *walk.Label
	var progress *walk.ProgressBar
	var action *walk.PushButton
	running := false
	complete := false
	var cancel context.CancelFunc
	var destructive atomic.Bool

	buttonText := "Install"
	description := "Install Metrolist Desktop and create its Windows shortcuts."
	if installed {
		if automatic {
			buttonText = "Update"
			description = "Install this update without changing your settings. Metrolist will restart afterward."
		} else {
			comparison, _ := installer.CompareVersions(buildconfig.TargetVersion, installedVersion)
			if comparison > 0 {
				buttonText = "Update"
				description = "Install this update without changing your settings. Metrolist will restart afterward."
			} else {
				buttonText = "Repair"
				description = "Reinstall the app without removing your settings or library."
			}
		}
	}

	start := func() {}
	declaration := MainWindow{
		AssignTo: &window,
		Title:    "Metrolist Desktop " + buttonText,
		MinSize:  Size{Width: 560, Height: 285},
		Size:     Size{Width: 620, Height: 320},
		Layout:   VBox{MarginsZero: false, SpacingZero: false},
		Children: []Widget{
			Label{Text: "Metrolist Desktop " + buildconfig.TargetVersion, Font: Font{PointSize: 16, Bold: true}},
			Label{Text: description},
			Label{Text: "Install location"},
			Composite{
				Layout: Grid{Columns: 2},
				Children: []Widget{
					LineEdit{AssignTo: &destination, Text: options.installRoot, ReadOnly: installed},
					PushButton{AssignTo: &browse, Text: "Browse...", Enabled: !installed, OnClicked: func() {
						dialog := new(walk.FileDialog)
						dialog.Title = "Choose Metrolist installation directory"
						dialog.FilePath = destination.Text()
						if accepted, err := dialog.ShowBrowseFolder(window); err == nil && accepted {
							destination.SetText(dialog.FilePath)
						}
					}},
				},
			},
			CheckBox{AssignTo: &desktop, Text: "Create a desktop shortcut", Checked: options.desktopShortcut, Enabled: !installed},
			CheckBox{AssignTo: &launch, Text: "Launch Metrolist Desktop when finished", Checked: options.launch, Enabled: !automatic},
			ProgressBar{AssignTo: &progress, MinValue: 0, MaxValue: 100},
			Label{AssignTo: &status, Text: "Ready"},
			PushButton{AssignTo: &action, Text: buttonText, OnClicked: func() {
				if complete {
					window.Close()
					return
				}
				if running {
					if cancel != nil && !destructive.Load() {
						cancel()
						action.SetEnabled(false)
						status.SetText("Canceling...")
					}
					return
				}
				start()
			}},
		},
	}
	if err := declaration.Create(); err != nil {
		fail(nil, false, err)
	}
	window.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if running {
			*canceled = true
		}
	})

	start = func() {
		if running {
			return
		}
		root := strings.TrimSpace(destination.Text())
		if root == "" {
			walk.MsgBox(window, "Metrolist Manager", "Choose an installation directory.", walk.MsgBoxIconWarning)
			return
		}
		running = true
		destructive.Store(false)
		operationContext, operationCancel := context.WithCancel(context.Background())
		cancel = operationCancel
		action.SetText("Cancel")
		action.SetEnabled(true)
		destination.SetEnabled(false)
		browse.SetEnabled(false)
		desktop.SetEnabled(false)
		launch.SetEnabled(false)
		operationOptions := options
		operationOptions.installRoot = root
		operationOptions.desktopShortcut = desktop.Checked()
		operationOptions.desktopSet = !installed || options.desktopSet
		operationOptions.launch = launch.Checked()
		go func() {
			lastProgress := -1
			err := runOperation(operationContext, operationOptions, func(event operation.Event) {
				if event.Stage == operation.StageInstalling {
					destructive.Store(true)
				}
				progressValue := -1
				if event.Total > 0 {
					progressValue = int(event.Downloaded * 100 / event.Total)
					if event.Stage == operation.StageDownloading && progressValue == lastProgress {
						return
					}
					lastProgress = progressValue
				}
				window.Synchronize(func() {
					status.SetText(string(event.Stage))
					if event.Stage == operation.StageInstalling {
						action.SetEnabled(false)
					}
					if progressValue >= 0 {
						progress.SetValue(progressValue)
					}
				})
			}, func() bool {
				confirmed := make(chan bool, 1)
				window.Synchronize(func() {
					confirmed <- walk.MsgBox(window, "Metrolist is still running", "Metrolist Desktop did not close within 30 seconds. Force it to close? Unsaved state may be lost.", walk.MsgBoxYesNo|walk.MsgBoxIconWarning) == walk.DlgCmdYes
				})
				return <-confirmed
			})
			operationCancel()
			window.Synchronize(func() {
				running = false
				cancel = nil
				if err != nil {
					action.SetText(buttonText)
					action.SetEnabled(true)
					destination.SetEnabled(!installed)
					browse.SetEnabled(!installed)
					desktop.SetEnabled(!installed)
					launch.SetEnabled(!automatic)
					if errors.Is(err, context.Canceled) {
						status.SetText("Canceled")
						return
					}
					status.SetText(buttonText + " failed")
					walk.MsgBox(window, "Metrolist Manager", err.Error(), walk.MsgBoxIconError)
					return
				}
				complete = true
				progress.SetValue(100)
				if operationOptions.launch {
					status.SetText("Metrolist is ready. Right-click its taskbar icon to pin it.")
				} else {
					status.SetText("Metrolist Desktop is ready.")
				}
				action.SetText("Close")
				action.SetEnabled(true)
			})
		}()
	}
	if automatic {
		window.Starting().Once(start)
	}
	window.Run()
}

func runOperation(ctx context.Context, options options, report func(operation.Event), confirmForce func() bool) error {
	installRoot, err := filepath.Abs(strings.TrimSpace(options.installRoot))
	if err != nil {
		return fmt.Errorf("resolve installation directory: %w", err)
	}
	options.installRoot = installRoot
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	desktopShortcut := (*bool)(nil)
	if options.desktopSet {
		desktopShortcut = &options.desktopShortcut
	}
	userAgent := "MetrolistManager/" + buildconfig.TargetVersion
	client := githubrelease.NewClient(userAgent)
	resolveAsset := func(ctx context.Context) (githubrelease.Asset, error) {
		return client.FindAsset(ctx, buildconfig.RepositoryOwner, buildconfig.RepositoryName, buildconfig.ReleaseTag, buildconfig.DesktopAssetName)
	}
	var ensureStopped func(context.Context) error
	if options.installed {
		ensureStopped = func(ctx context.Context) error {
			return ensureAppStopped(ctx, options.installRoot, options.force, confirmForce)
		}
	}
	return operation.Run(ctx, operation.Request{
		Version:         buildconfig.TargetVersion,
		InstallRoot:     options.installRoot,
		ManagerSource:   executable,
		DesktopShortcut: desktopShortcut,
		LaunchAfter:     options.launch,
	}, operation.Dependencies{
		ResolveAsset:  resolveAsset,
		Downloader:    client,
		Registrar:     winplatform.Registrar{},
		EnsureStopped: ensureStopped,
		Launch:        winplatform.Launch,
	}, report)
}

func runUninstallUI(options options) {
	root, _, err := winplatform.LoadInstallation()
	if err != nil {
		fail(nil, false, err)
	}
	var window *walk.MainWindow
	var purge *walk.CheckBox
	declaration := MainWindow{
		AssignTo: &window,
		Title:    "Uninstall Metrolist Desktop",
		MinSize:  Size{Width: 480, Height: 200},
		Size:     Size{Width: 520, Height: 220},
		Layout:   VBox{},
		Children: []Widget{
			Label{Text: "Remove Metrolist Desktop", Font: Font{PointSize: 15, Bold: true}},
			Label{Text: root, EllipsisMode: EllipsisPath},
			CheckBox{AssignTo: &purge, Text: "Also remove settings, downloads, and cached data", Checked: options.purgeData},
			Composite{Layout: HBox{}, Children: []Widget{
				HSpacer{},
				PushButton{Text: "Cancel", OnClicked: func() { window.Close() }},
				PushButton{Text: "Uninstall", OnClicked: func() {
					if walk.MsgBox(window, "Uninstall Metrolist Desktop", "Remove Metrolist Desktop from this computer?", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
						return
					}
					if err := startUninstall(purge.Checked(), options.force, false, func() bool {
						return walk.MsgBox(window, "Metrolist is running", "Metrolist Desktop is still running. Force it to close before uninstalling? Unsaved state may be lost.", walk.MsgBoxYesNo|walk.MsgBoxIconWarning) == walk.DlgCmdYes
					}); err != nil {
						walk.MsgBox(window, "Metrolist Manager", err.Error(), walk.MsgBoxIconError)
						return
					}
					window.Close()
				}},
			}},
		},
	}
	if _, err := declaration.Run(); err != nil {
		fail(nil, false, err)
	}
}

func startUninstall(purge, force, silent bool, confirmForce func() bool) error {
	root, _, err := winplatform.LoadInstallation()
	if err != nil {
		return err
	}
	marker, err := installer.ReadMarker(root)
	if err != nil {
		return fmt.Errorf("read installation identity: %w", err)
	}
	appPIDs, err := winplatform.FindRunningApps(root)
	if err != nil {
		return fmt.Errorf("check for running Metrolist Desktop: %w", err)
	}
	if len(appPIDs) != 0 {
		if !force && confirmForce != nil {
			force = confirmForce()
		}
		if !force {
			return errors.New("Metrolist Desktop is still running; close it or use --force")
		}
		if err := winplatform.TerminateRunningApps(root); err != nil {
			return fmt.Errorf("close Metrolist Desktop: %w", err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp("", "MetrolistManager-uninstall-*.exe")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	temporaryStarted := false
	defer func() {
		if !temporaryStarted {
			os.Remove(temporaryPath)
		}
	}()
	source, err := os.Open(executable)
	if err != nil {
		temporary.Close()
		return err
	}
	_, copyErr := io.Copy(temporary, source)
	sourceCloseErr := source.Close()
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if copyErr != nil || sourceCloseErr != nil || syncErr != nil || closeErr != nil {
		return errors.Join(copyErr, sourceCloseErr, syncErr, closeErr)
	}
	if err := installer.RemoveLauncherForReplacement(root); err != nil {
		return fmt.Errorf("prevent Metrolist Desktop from restarting during uninstall: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(root, installer.AppDirectoryName)); err != nil {
		return fmt.Errorf("remove Metrolist Desktop application files: %w", err)
	}
	if purge {
		closeLogging()
		if err := winplatform.RemoveUserData(); err != nil {
			return err
		}
	}
	if err := winplatform.Unregister(); err != nil {
		return err
	}
	arguments := []string{
		"uninstall-worker",
		"--install-dir", root,
		"--parent-pid", strconv.Itoa(os.Getpid()),
		"--installation-id", marker.InstallationID,
	}
	if silent {
		arguments = append(arguments, "--silent")
	}
	command := exec.Command(temporaryPath, arguments...)
	if err := command.Start(); err != nil {
		restoreErr := (winplatform.Registrar{}).Register(root, marker.Version, marker.DesktopShortcut)
		return errors.Join(fmt.Errorf("start uninstall cleanup worker: %w", err), restoreErr)
	}
	temporaryStarted = true
	if err := command.Process.Release(); err != nil {
		return err
	}
	return nil
}

func runUninstallWorker(options options) error {
	if options.installRoot == "" || options.parentPID == 0 || options.installationID == "" {
		return errors.New("uninstall worker is missing required arguments")
	}
	root, err := filepath.Abs(options.installRoot)
	if err != nil {
		return fmt.Errorf("resolve uninstall directory: %w", err)
	}
	worker, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate uninstall worker: %w", err)
	}
	if err := validateUninstallWorkerPath(root, worker); err != nil {
		return err
	}
	if err := validateUninstallCleanupTarget(root, options.installationID); err != nil {
		return err
	}
	defer scheduleSelfRemoval()
	exited, err := winplatform.WaitForProcess(options.parentPID, 30*time.Second)
	if err != nil {
		return fmt.Errorf("wait for uninstaller: %w", err)
	}
	if !exited {
		return errors.New("the installed uninstaller did not exit within 30 seconds")
	}
	releaseLock, err := winplatform.AcquireManagerLock()
	if err != nil {
		return err
	}
	defer releaseLock()
	if err := validateUninstallCleanupTarget(root, options.installationID); err != nil {
		return err
	}
	if err := retryFilesystemOperation(func() error {
		return os.RemoveAll(filepath.Join(root, installer.ManagerDirectoryName))
	}); err != nil {
		return fmt.Errorf("remove Metrolist Manager files: %w", err)
	}
	if err := retryFilesystemOperation(func() error {
		err := os.Remove(root)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}); err != nil {
		return fmt.Errorf("remove installation directory: %w", err)
	}
	return nil
}

func validateUninstallCleanupTarget(root, installationID string) error {
	marker, err := installer.ReadMarker(root)
	if err != nil {
		return fmt.Errorf("verify uninstall identity: %w", err)
	}
	if marker.InstallationID != installationID {
		return errors.New("uninstall identity does not match the installation")
	}
	if _, err := os.Stat(filepath.Join(root, installer.AppDirectoryName)); err == nil {
		return errors.New("Metrolist Desktop was reinstalled before uninstall cleanup completed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check for a reinstalled application: %w", err)
	}
	registeredRoot, _, err := winplatform.LoadInstallation()
	if err == nil && samePath(registeredRoot, root) {
		return errors.New("Metrolist Desktop was registered again before uninstall cleanup completed")
	}
	if err != nil && !errors.Is(err, winplatform.ErrNotInstalled) {
		return fmt.Errorf("check for a new installation registration: %w", err)
	}
	return nil
}

func retryFilesystemOperation(operation func() error) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		if err = operation(); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

func validateUninstallWorkerPath(root, worker string) error {
	installedManager := filepath.Join(root, installer.ManagerDirectoryName, installer.ManagerExecutable)
	if samePath(worker, installedManager) {
		return errors.New("the installed manager cannot run as the uninstall worker")
	}
	workerRelative, err := filepath.Rel(os.TempDir(), worker)
	if err != nil || workerRelative == "." || workerRelative == ".." || strings.HasPrefix(workerRelative, ".."+string(filepath.Separator)) {
		return errors.New("uninstall worker is not in the temporary directory")
	}
	workerName := filepath.Base(worker)
	if !strings.HasPrefix(workerName, "MetrolistManager-uninstall-") || !strings.HasSuffix(strings.ToLower(workerName), ".exe") {
		return errors.New("uninstall worker has an invalid file name")
	}
	return nil
}

func scheduleSelfRemoval() {
	executable, err := os.Executable()
	if err != nil {
		return
	}
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return
	}
	powershell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	command := exec.Command(
		powershell,
		"-NoProfile",
		"-NonInteractive",
		"-WindowStyle",
		"Hidden",
		"-Command",
		`Wait-Process -Id $env:METROLIST_CLEANUP_PID -ErrorAction SilentlyContinue; Remove-Item -LiteralPath $env:METROLIST_CLEANUP_PATH -Force -ErrorAction SilentlyContinue`,
	)
	command.Env = append(os.Environ(), "METROLIST_CLEANUP_PID="+strconv.Itoa(os.Getpid()), "METROLIST_CLEANUP_PATH="+executable)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if command.Start() == nil {
		_ = command.Process.Release()
	}
}

func ensureAppStopped(ctx context.Context, root string, force bool, confirmForce func() bool) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wait := min(remaining, 500*time.Millisecond)
		exited, err := winplatform.WaitForApps(root, wait)
		if err != nil {
			return fmt.Errorf("wait for Metrolist Desktop: %w", err)
		}
		if exited {
			processes, err := winplatform.FindRunningApps(root)
			if err != nil {
				return fmt.Errorf("check for restarted Metrolist Desktop: %w", err)
			}
			if len(processes) == 0 {
				return nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !force && confirmForce != nil {
		force = confirmForce()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !force {
		return errors.New("Metrolist Desktop is still running")
	}
	if err := winplatform.TerminateRunningApps(root); err != nil {
		return fmt.Errorf("close Metrolist Desktop: %w", err)
	}
	remaining, err := winplatform.FindRunningApps(root)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return errors.New("Metrolist Desktop restarted while the manager was preparing the installation")
	}
	return nil
}

func setupLogging() {
	cache, err := os.UserCacheDir()
	if err != nil {
		return
	}
	directory := filepath.Join(cache, "Metrolist", "Manager")
	if os.MkdirAll(directory, 0o755) != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(directory, "manager.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		managerLog = file
		log.SetOutput(file)
	}
}

func closeLogging() {
	if managerLog != nil {
		_ = managerLog.Close()
		managerLog = nil
		log.SetOutput(io.Discard)
	}
}

func fail(owner walk.Form, silent bool, err error) {
	log.Printf("fatal: %v", err)
	if !silent {
		walk.MsgBox(owner, "Metrolist Manager", err.Error(), walk.MsgBoxIconError)
	}
	os.Exit(1)
}

func samePath(left, right string) bool {
	leftInfo, leftStatErr := os.Stat(left)
	rightInfo, rightStatErr := os.Stat(right)
	if leftStatErr == nil && rightStatErr == nil {
		return os.SameFile(leftInfo, rightInfo)
	}
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && strings.EqualFold(filepath.Clean(leftAbsolute), filepath.Clean(rightAbsolute))
}
