package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lynxbase/lynxdb/internal/ui"
	"github.com/lynxbase/lynxdb/pkg/client"
)

func init() {
	rootCmd.AddCommand(newMVCmd())
}

func newMVCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mv",
		Short: "Manage materialized views",
		Example: `  lynxdb mv create mv_errors_5m 'level=error | stats count by source' --retention 90d
  lynxdb mv list
  lynxdb mv status mv_errors_5m
  lynxdb mv backfill mv_errors_5m
  lynxdb mv pause mv_errors_5m
  lynxdb mv resume mv_errors_5m
  lynxdb mv drop mv_errors_5m`,
	}

	var retention string

	createCmd := &cobra.Command{
		Use:   "create <name> <query>",
		Short: "Create a materialized view",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return runMVCreate(args[0], args[1], retention)
		},
	}
	createCmd.Flags().StringVar(&retention, "retention", "", "Retention period (e.g., 30d)")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all materialized views",
		RunE:  runMVList,
	}
	statusCmd := &cobra.Command{
		Use:               "status <name>",
		Short:             "Show detailed view status",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMVNames,
		RunE: func(_ *cobra.Command, args []string) error {
			return runMVStatus(args[0])
		},
	}

	var forceFlag bool
	var dryRunFlag bool

	dropCmd := &cobra.Command{
		Use:               "drop <name>",
		Short:             "Drop a materialized view",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMVNames,
		RunE: func(_ *cobra.Command, args []string) error {
			return runMVDrop(args[0], forceFlag, dryRunFlag)
		},
	}
	dropCmd.Flags().BoolVar(&forceFlag, "force", false, "Skip confirmation prompt")
	dropCmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "Show what would be deleted without applying")

	pauseCmd := &cobra.Command{
		Use:               "pause <name>",
		Short:             "Pause a materialized view pipeline",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMVNames,
		RunE: func(_ *cobra.Command, args []string) error {
			return runMVPause(args[0])
		},
	}

	resumeCmd := &cobra.Command{
		Use:               "resume <name>",
		Short:             "Resume a paused materialized view pipeline",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMVNames,
		RunE: func(_ *cobra.Command, args []string) error {
			return runMVResume(args[0])
		},
	}

	backfillCmd := &cobra.Command{
		Use:               "backfill <name>",
		Short:             "Manually trigger a backfill for a materialized view",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMVNames,
		RunE: func(_ *cobra.Command, args []string) error {
			return runMVBackfill(args[0])
		},
	}

	var migrateAllFlag bool
	var migrateDryRunFlag bool
	var migrateQueryFlag string

	migrateCmd := &cobra.Command{
		Use:   "migrate [name]",
		Short: "Migrate materialized view queries from SPL2 to LynxFlow",
		Long: `Migrate materialized view queries from SPL2 to LynxFlow.

Since the SPL2-to-LynxFlow auto-translator has been removed, you must
provide the replacement LynxFlow query explicitly with --query.

With a name argument, migrates a single view. With --all --dry-run,
lists all views that need migration along with their current queries.`,
		Example: `  lynxdb mv migrate mv_errors_5m --query 'from main | where level == "error" | stats count() by service'
  lynxdb mv migrate mv_errors_5m --dry-run
  lynxdb mv migrate --all --dry-run`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeMVNames,
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return runMVMigrate(name, migrateAllFlag, migrateDryRunFlag, migrateQueryFlag)
		},
	}
	migrateCmd.Flags().BoolVar(&migrateAllFlag, "all", false, "Migrate all SPL2 views")
	migrateCmd.Flags().BoolVar(&migrateDryRunFlag, "dry-run", false, "Show translations without applying")
	migrateCmd.Flags().StringVar(&migrateQueryFlag, "query", "", "LynxFlow replacement query for the view")

	cmd.AddCommand(createCmd, listCmd, statusCmd, dropCmd, pauseCmd, resumeCmd, backfillCmd, migrateCmd)

	return cmd
}

func runMVCreate(name, query, retention string) error {
	ctx := context.Background()

	// Pre-validate query so parse errors get caret display.
	if _, err := apiClient().Explain(ctx, query); err != nil {
		if client.IsInvalidQuery(err) {
			return &queryError{inner: err, query: query}
		}
		// Non-parse errors — proceed to create and let the server report them.
	}

	input := client.ViewInput{
		Name: name,
		Q:    query,
	}
	if retention != "" {
		input.Retention = retention
	}

	if _, err := apiClient().CreateView(ctx, input); err != nil {
		return err
	}

	printSuccess("Created materialized view %q", name)
	printNextSteps(
		fmt.Sprintf("lynxdb mv status %s        Track backfill progress", name),
		"lynxdb mv list                  List all views",
	)

	return nil
}

func runMVList(_ *cobra.Command, _ []string) error {
	ctx := context.Background()

	views, err := apiClient().ListViews(ctx)
	if err != nil {
		return err
	}

	if isJSONFormat() {
		for _, v := range views {
			b, _ := json.Marshal(v)
			fmt.Println(string(b))
		}

		return nil
	}

	if len(views) == 0 {
		if !humanOutputActive() {
			return renderTabular(os.Stdout, []string{"NAME", "STATUS", "QUERY"}, nil, ui.Stdout)
		}
		fmt.Println("No materialized views.")
		printNextSteps(
			"lynxdb mv create <name> <query>   Create a new view",
		)

		return nil
	}

	t := ui.Stdout
	rows := make([][]any, 0, len(views))
	for _, v := range views {
		status := v.Status
		if humanOutputActive() {
			status = mvStatusColored(t, v.Status)
		}
		rows = append(rows, []any{v.Name, status, v.Query})
	}

	if err := renderTabular(os.Stdout, []string{"NAME", "STATUS", "QUERY"}, rows, t); err != nil {
		return err
	}
	if humanOutputActive() {
		fmt.Printf("\n%s\n", t.Dim.Render(fmt.Sprintf("%d views total", len(views))))
	}

	return nil
}

func runMVStatus(name string) error {
	ctx := context.Background()

	view, err := apiClient().GetView(ctx, name)
	if err != nil {
		return err
	}

	if isJSONFormat() {
		b, _ := json.MarshalIndent(view, "", "  ")
		fmt.Println(string(b))

		return nil
	}

	if !humanOutputActive() {
		rows := [][2]any{
			{"name", view.Name},
			{"status", view.Status},
			{"query", view.Query},
			{"retention", view.Retention},
			{"created", view.CreatedAt},
		}
		if view.Backfill != nil {
			rows = append(rows,
				[2]any{"backfill_phase", view.Backfill.Phase},
				[2]any{"backfill_segments_scanned", view.Backfill.SegmentsScanned},
				[2]any{"backfill_segments_total", view.Backfill.SegmentsTotal},
				[2]any{"backfill_rows_scanned", view.Backfill.RowsScanned},
				[2]any{"backfill_elapsed_ms", view.Backfill.ElapsedMS},
			)
		}
		return renderKeyValues(os.Stdout, rows, ui.Stdout)
	}

	t := ui.Stdout

	fmt.Println()
	fmt.Printf("  %s\n\n", t.Bold.Render(view.Name))
	fmt.Println(t.KeyValue("Status", mvStatusColored(t, view.Status)))

	if lower := strings.ToLower(view.Status); lower == "backfill" || lower == "backfilling" {
		if view.Backfill != nil {
			elapsed := time.Duration(view.Backfill.ElapsedMS * float64(time.Millisecond))
			fmt.Println(t.KeyValue("Progress", fmt.Sprintf("%s — %d/%d segments, %s rows scanned (%s)",
				view.Backfill.Phase,
				view.Backfill.SegmentsScanned, view.Backfill.SegmentsTotal,
				formatCountHuman(view.Backfill.RowsScanned),
				formatElapsed(elapsed))))
		} else {
			fmt.Println(t.KeyValue("Progress", t.Dim.Render("starting...")))
		}
	}

	fmt.Println(t.KeyValue("Query", view.Query))

	if len(view.Columns) > 0 {
		names := make([]string, 0, len(view.Columns))
		for _, c := range view.Columns {
			names = append(names, c.Name)
		}

		fmt.Println(t.KeyValue("Columns", strings.Join(names, ", ")))
	}

	fmt.Println(t.KeyValue("Retention", view.Retention))
	fmt.Println(t.KeyValue("Created", view.CreatedAt))
	fmt.Println()

	lower := strings.ToLower(view.Status)
	switch lower {
	case "backfill":
		printNextSteps(
			fmt.Sprintf("lynxdb mv status %s         Check backfill progress", name),
			fmt.Sprintf("lynxdb query '| from %s'    Query the view (partial results during backfill)", name),
		)
	default:
		printNextSteps(
			fmt.Sprintf("lynxdb mv pause %s          Pause the pipeline", name),
			fmt.Sprintf("lynxdb query '| from %s'    Query the view", name),
		)
	}

	return nil
}

func runMVDrop(name string, force, dryRun bool) error {
	if dryRun {
		t := ui.Stdout
		fmt.Printf("  %s\n", t.Bold.Render("Would delete:"))
		fmt.Println(t.KeyValue("View", name))
		fmt.Printf("\n  %s\n", t.Dim.Render("Run without --dry-run to delete."))

		return nil
	}

	if !force {
		msg := fmt.Sprintf("This will permanently delete materialized view '%s' and all its data.", name)
		if !confirmDestructive(msg, name) {
			if !isStdinTTY() {
				return fmt.Errorf("destructive action requires confirmation; use --force in non-interactive mode")
			}

			printHint("Aborted.")

			return nil
		}
	}

	ctx := context.Background()
	if err := apiClient().DeleteView(ctx, name); err != nil {
		return err
	}

	printSuccess("Dropped materialized view %q", name)

	return nil
}

// runMVBackfill triggers a manual backfill for a materialized view.
func runMVBackfill(name string) error {
	ctx := context.Background()

	if err := apiClient().TriggerBackfill(ctx, name); err != nil {
		return err
	}

	printSuccess("Backfill triggered for materialized view %q", name)
	printNextSteps(
		fmt.Sprintf("lynxdb mv status %s         Track backfill progress", name),
		fmt.Sprintf("lynxdb query '| from %s'    Query the view", name),
	)

	return nil
}

// runMVPause pauses a materialized view pipeline.
func runMVPause(name string) error {
	return patchMVPaused(name, true)
}

// runMVResume resumes a paused materialized view pipeline.
func runMVResume(name string) error {
	return patchMVPaused(name, false)
}

// patchMVPaused sends a PATCH request to pause or resume a materialized view.
func patchMVPaused(name string, paused bool) error {
	ctx := context.Background()

	if _, err := apiClient().PatchView(ctx, name, client.ViewPatchInput{
		Paused: &paused,
	}); err != nil {
		return err
	}

	if paused {
		printSuccess("Paused materialized view %q", name)
		printNextSteps(
			fmt.Sprintf("lynxdb mv resume %s   Resume the pipeline", name),
			fmt.Sprintf("lynxdb mv status %s   Check current status", name),
		)
	} else {
		printSuccess("Resumed materialized view %q", name)
		printNextSteps(
			fmt.Sprintf("lynxdb mv status %s   Check current status", name),
		)
	}

	return nil
}

// runMVMigrate migrates materialized view queries from SPL2 to LynxFlow.
// The auto-translator was removed in RFC-002 Phase 10, so the user must
// supply the replacement LynxFlow query explicitly via --query.
func runMVMigrate(name string, all, dryRun bool, query string) error {
	ctx := context.Background()

	// Neither name nor --all: error.
	if name == "" && !all {
		return fmt.Errorf("specify a view name or --all")
	}

	// --all mode: list views that need migration.
	if all {
		return runMVMigrateAll(ctx, dryRun)
	}

	// Single-view mode with --dry-run.
	if dryRun {
		return runMVMigrateDryRun(ctx, name, query)
	}

	// Single-view mode without --query: show current query and instruct.
	if query == "" {
		return runMVMigrateNoQuery(ctx, name)
	}

	// Single-view mode with --query: validate and apply.
	return runMVMigrateApply(ctx, name, query)
}

// runMVMigrateAll lists all views that need migration.
func runMVMigrateAll(ctx context.Context, dryRun bool) error {
	views, err := apiClient().ListViews(ctx)
	if err != nil {
		return err
	}

	// Filter to views that need migration (status "needs-migration" or
	// any status where the query hasn't been converted to LynxFlow yet).
	var pending []client.View
	for _, v := range views {
		if strings.ToLower(v.Status) == "needs-migration" {
			pending = append(pending, v)
		}
	}

	if len(pending) == 0 {
		printSuccess("All materialized views are already using LynxFlow")
		return nil
	}

	t := ui.Stdout
	rows := make([][]any, 0, len(pending))
	for _, v := range pending {
		status := v.Status
		if humanOutputActive() {
			status = mvStatusColored(t, v.Status)
		}
		rows = append(rows, []any{v.Name, status, v.Query})
	}

	if err := renderTabular(os.Stdout, []string{"NAME", "STATUS", "CURRENT QUERY"}, rows, t); err != nil {
		return err
	}

	if humanOutputActive() {
		fmt.Printf("\n%s\n", t.Dim.Render(fmt.Sprintf("%d view(s) need migration", len(pending))))
	}

	if !dryRun {
		fmt.Println()
		printHint("Auto-translation has been removed. Migrate each view individually:")
		printNextSteps(
			"lynxdb mv migrate <name> --query '<lynxflow query>'",
		)
	}

	return nil
}

// runMVMigrateDryRun shows a view's current state and what it would be changed to.
func runMVMigrateDryRun(ctx context.Context, name, query string) error {
	view, err := apiClient().GetView(ctx, name)
	if err != nil {
		return err
	}

	t := ui.Stdout
	fmt.Println()
	fmt.Printf("  %s\n\n", t.Bold.Render(name))
	fmt.Println(t.KeyValue("Status", mvStatusColored(t, view.Status)))
	fmt.Println(t.KeyValue("Current query", view.Query))

	if query != "" {
		// Validate the proposed replacement query.
		if _, err := apiClient().Explain(ctx, query); err != nil {
			if client.IsInvalidQuery(err) {
				fmt.Println()
				fmt.Println(t.KeyValue("New query", query))
				fmt.Printf("\n  %s\n", t.Error.Render("Replacement query has parse errors:"))
				return &queryError{inner: err, query: query}
			}
		}
		fmt.Println(t.KeyValue("New query", query))
		fmt.Printf("\n  %s\n", t.Dim.Render("Run without --dry-run to apply."))
	} else {
		fmt.Printf("\n  %s\n", t.Dim.Render("Provide --query to preview the replacement."))
	}
	fmt.Println()

	return nil
}

// runMVMigrateNoQuery prints an error explaining --query is required and
// shows the view's current query so the user can hand-translate it.
func runMVMigrateNoQuery(ctx context.Context, name string) error {
	view, err := apiClient().GetView(ctx, name)
	if err != nil {
		return err
	}

	return fmt.Errorf("--query is required (auto-translation has been removed)\n\n"+
		"Current SPL2 query for '%s':\n"+
		"  %s\n\n"+
		"Translate this to LynxFlow and run:\n"+
		"  lynxdb mv migrate %s --query '<lynxflow query>'",
		name, view.Query, name)
}

// runMVMigrateApply validates the replacement query and patches the view.
func runMVMigrateApply(ctx context.Context, name, query string) error {
	// Fetch the current view to capture the old query.
	view, err := apiClient().GetView(ctx, name)
	if err != nil {
		return err
	}
	oldQuery := view.Query

	// Pre-validate the replacement query so parse errors get caret display.
	if _, err := apiClient().Explain(ctx, query); err != nil {
		if client.IsInvalidQuery(err) {
			return &queryError{inner: err, query: query}
		}
		// Non-parse errors — proceed to patch and let the server report them.
	}

	langVersion := "lynxflow"
	if _, err := apiClient().PatchView(ctx, name, client.ViewPatchInput{
		Query:           &query,
		LanguageVersion: &langVersion,
		MigratedFrom:    &oldQuery,
	}); err != nil {
		return err
	}

	printSuccess("Migrated materialized view %q", name)
	t := ui.Stdout
	fmt.Println(t.KeyValue("Old query", oldQuery))
	fmt.Println(t.KeyValue("New query", query))
	fmt.Println()

	printNextSteps(
		fmt.Sprintf("lynxdb mv status %s   Check view status", name),
	)

	return nil
}

// mvStatusColored returns a colored status string for TTY display.
func mvStatusColored(t *ui.Theme, status string) string {
	lower := strings.ToLower(status)
	switch {
	case lower == "active":
		return t.Success.Render(status)
	case lower == "backfilling" || lower == "backfill":
		return t.Warning.Render(status)
	case lower == "paused":
		return t.Dim.Render(status)
	case lower == "needs-migration":
		return t.Warning.Render(status)
	case lower == "error" || strings.HasPrefix(lower, "err"):
		return t.Error.Render(status)
	default:
		return status
	}
}
