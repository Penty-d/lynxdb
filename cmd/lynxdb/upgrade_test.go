package main

import (
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/internal/upgrade"
)

func TestUpgradeCommandDefaultChannel(t *testing.T) {
	cmd := newUpgradeCmd()
	flag := cmd.Flags().Lookup("channel")
	if flag == nil {
		t.Fatal("channel flag missing")
	}
	if flag.DefValue != upgrade.ChannelStable {
		t.Fatalf("channel default = %q, want %q", flag.DefValue, upgrade.ChannelStable)
	}
}

func TestValidateUpgradeOptions(t *testing.T) {
	tests := []struct {
		name             string
		channel          string
		version          string
		allowPrerelease  bool
		wantErrSubstring string
	}{
		{
			name:    "stable default",
			channel: upgrade.ChannelStable,
		},
		{
			name:             "nightly requires allow",
			channel:          upgrade.ChannelNightly,
			wantErrSubstring: "nightly upgrades require",
		},
		{
			name:            "nightly with allow",
			channel:         upgrade.ChannelNightly,
			allowPrerelease: true,
		},
		{
			name:             "explicit prerelease requires allow",
			channel:          upgrade.ChannelStable,
			version:          "v0.7.0-nightly.20260509.g1a2b3c4",
			wantErrSubstring: "prerelease versions require",
		},
		{
			name:            "explicit prerelease with allow",
			channel:         upgrade.ChannelStable,
			version:         "v0.7.0-nightly.20260509.g1a2b3c4",
			allowPrerelease: true,
		},
		{
			name:             "unknown channel",
			channel:          "beta",
			wantErrSubstring: "invalid channel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUpgradeOptions(tt.channel, tt.version, tt.allowPrerelease)
			if tt.wantErrSubstring == "" {
				if err != nil {
					t.Fatalf("validateUpgradeOptions() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateUpgradeOptions() error = nil, want %q", tt.wantErrSubstring)
			}
			if !strings.Contains(err.Error(), tt.wantErrSubstring) {
				t.Fatalf("validateUpgradeOptions() error = %q, want substring %q", err, tt.wantErrSubstring)
			}
		})
	}
}

func TestUpgradeChannelForTarget(t *testing.T) {
	got := upgradeChannelForTarget(upgrade.ChannelStable, "v0.7.0-nightly.20260509.g1a2b3c4")
	if got != upgrade.ChannelNightly {
		t.Fatalf("nightly target channel = %q, want %q", got, upgrade.ChannelNightly)
	}

	got = upgradeChannelForTarget(upgrade.ChannelStable, "v0.7.0")
	if got != upgrade.ChannelStable {
		t.Fatalf("stable target channel = %q, want %q", got, upgrade.ChannelStable)
	}
}
