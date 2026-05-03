package main

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(newAdminCmd())
}

func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administer local LynxDB storage",
	}
	cmd.AddCommand(newAdminFormatUpgradeCmd())
	return cmd
}
