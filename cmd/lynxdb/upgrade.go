package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lynxbase/lynxdb/internal/buildinfo"
	"github.com/lynxbase/lynxdb/internal/upgrade"
)

func init() {
	rootCmd.AddCommand(newUpgradeCmd())
}

func newUpgradeCmd() *cobra.Command {
	var (
		flagCheck           bool
		flagVersion         string
		flagForce           bool
		flagYes             bool
		flagChannel         string
		flagAllowPrerelease bool
	)

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade LynxDB to the latest version",
		Long: `Check for and install LynxDB updates.

By default, upgrades to the latest stable release. Use --channel nightly with
--allow-prerelease to opt in to nightly builds. Use --version to install a
specific version, or --check to check without installing.`,
		Example: `  lynxdb upgrade              # Upgrade to latest
  lynxdb upgrade --check      # Check only, don't install
  lynxdb upgrade --version v0.6.0  # Install specific version
  lynxdb upgrade --channel nightly --allow-prerelease
  lynxdb upgrade --yes         # Skip confirmation prompt`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runUpgrade(flagCheck, flagVersion, flagForce, flagYes, flagChannel, flagAllowPrerelease)
		},
	}

	cmd.Flags().BoolVar(&flagCheck, "check", false, "Check for updates without installing")
	cmd.Flags().StringVar(&flagVersion, "version", "", "Install a specific version (e.g., v0.6.0)")
	cmd.Flags().StringVar(&flagChannel, "channel", upgrade.ChannelStable, "Release channel: stable or nightly")
	cmd.Flags().BoolVar(&flagAllowPrerelease, "allow-prerelease", false, "Allow nightly and explicit prerelease versions")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Skip 'already up to date' check")
	cmd.Flags().BoolVar(&flagYes, "yes", false, "Skip confirmation prompt")

	return cmd
}

func runUpgrade(check bool, version string, force, yes bool, channel string, allowPrerelease bool) error {
	if err := validateUpgradeOptions(channel, version, allowPrerelease); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if buildinfo.IsDev() && !force {
		printWarning("Development build detected. Use 'go install' to update, or pass --force to override.")
		return nil
	}

	if version == "" {
		printMeta("Checking for %s updates...", channel)
	} else {
		printMeta("Checking version %s...", version)
	}

	var result *upgrade.CheckResult
	var err error

	if version != "" {
		manifest, fetchErr := upgrade.FetchVersionedManifest(ctx, version)
		if fetchErr != nil {
			return fmt.Errorf("failed to fetch version info: %w", fetchErr)
		}
		result, err = upgrade.CheckAgainstManifest(manifest, buildinfo.Version)
		if err != nil {
			if errors.Is(err, upgrade.ErrPlatformNotFound) {
				return fmt.Errorf("no build for %s/%s in %s. Check available builds at https://github.com/lynxbase/lynxdb/releases/%s",
					runtime.GOOS, runtime.GOARCH, version, version)
			}
			return err
		}
		// Treat explicit --version as an update even for downgrades.
		if result.LatestVersion != buildinfo.Version {
			result.UpdateAvail = true
			// Re-fetch the artifact for this platform.
			if result.Artifact == nil {
				key := upgrade.PlatformKey()
				if a, ok := manifest.Artifacts[key]; ok {
					result.Artifact = &a
				}
			}
		}
	} else {
		result, err = upgrade.CheckChannel(ctx, buildinfo.Version, channel)
		if err != nil {
			if errors.Is(err, upgrade.ErrPlatformNotFound) {
				return fmt.Errorf("no build for %s/%s. Check available builds at https://github.com/lynxbase/lynxdb/releases",
					runtime.GOOS, runtime.GOARCH)
			}
			return fmt.Errorf("update check failed: %w", err)
		}
	}

	if !result.UpdateAvail && !force {
		printSuccess("Already up to date (%s)", buildinfo.Version)
		return nil
	}

	if check {
		if result.UpdateAvail {
			printSuccess("Update available: %s -> %s", result.CurrentVersion, result.LatestVersion)
			if result.ChangelogURL != "" {
				printMeta("Changelog: %s", result.ChangelogURL)
			}
			for _, notice := range result.Notices {
				printWarning("%s", notice)
			}
			// Exit code 1 = update available (for scripting).
			os.Exit(1)
		}
		printSuccess("Already up to date (%s)", buildinfo.Version)
		return nil
	}

	for _, notice := range result.Notices {
		printWarning("%s", notice)
	}

	targetVersion := result.LatestVersion
	if version != "" {
		targetVersion = version
	}
	resolvedChannel := upgradeChannelForTarget(channel, targetVersion)

	if isTTY() && !yes {
		msg := fmt.Sprintf("Upgrade LynxDB %s -> %s?", buildinfo.Version, targetVersion)
		if resolvedChannel == upgrade.ChannelNightly {
			msg = fmt.Sprintf("Upgrade LynxDB %s -> %s from nightly?", buildinfo.Version, targetVersion)
		}
		if !confirmAction(msg) {
			printHint("Aborted.")
			os.Exit(exitAborted)
		}
	}

	if result.Artifact == nil {
		return fmt.Errorf("no artifact available for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	printMeta("Downloading lynxdb %s for %s/%s...", targetVersion, runtime.GOOS, runtime.GOARCH)

	var lastPercent int
	progressFn := func(downloaded, total int64) {
		if total > 0 {
			percent := int(downloaded * 100 / total)
			if percent != lastPercent && percent%10 == 0 {
				printMeta("  %s / %s (%d%%)", formatBytes(downloaded), formatBytes(total), percent)
				lastPercent = percent
			}
		}
	}

	archivePath, err := upgrade.DownloadWithProgress(ctx, result.Artifact, progressFn)
	if err != nil {
		if errors.Is(err, upgrade.ErrChecksumMismatch) {
			printWarning("Downloaded file checksum does not match. The file may be corrupted or tampered with.")
			printWarning("Please try again or download manually from GitHub.")
			os.Exit(1)
		}
		return fmt.Errorf("download failed: %w", err)
	}
	defer os.Remove(archivePath)

	printSuccess("Checksum verified")

	if err := upgrade.Install(archivePath); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied. Try: sudo lynxdb upgrade")
		}
		return fmt.Errorf("installation failed: %w", err)
	}

	printSuccess("Upgraded: %s -> %s", buildinfo.Version, targetVersion)
	printNextSteps("Restart any running lynxdb processes")

	return nil
}

func validateUpgradeOptions(channel, version string, allowPrerelease bool) error {
	switch channel {
	case upgrade.ChannelStable, upgrade.ChannelNightly:
	default:
		return fmt.Errorf("invalid channel %q: use stable or nightly", channel)
	}

	if channel == upgrade.ChannelNightly && !allowPrerelease {
		return fmt.Errorf("nightly upgrades require --channel nightly --allow-prerelease")
	}

	if isPrereleaseVersion(version) && !allowPrerelease {
		return fmt.Errorf("prerelease versions require --allow-prerelease")
	}

	return nil
}

func isPrereleaseVersion(version string) bool {
	for _, r := range version {
		if r == '-' {
			return true
		}
	}
	return false
}

func upgradeChannelForTarget(channel, version string) string {
	if isNightlyVersion(version) {
		return upgrade.ChannelNightly
	}
	return channel
}

func isNightlyVersion(version string) bool {
	return strings.Contains(version, "-nightly.")
}
