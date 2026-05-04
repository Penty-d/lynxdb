package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/spf13/cobra"
)

var shipperConfigRemote string

//go:embed shippers/fixtures/*
var shipperConfigFS embed.FS

type shipperTemplateData struct {
	Remote string
}

var shipperConfigTemplatePaths = map[string]string{
	"filebeat":   "shippers/fixtures/filebeat.yml",
	"fluent-bit": "shippers/fixtures/fluentbit.conf",
	"vector":     "shippers/fixtures/vector.yaml",
	"otelcol":    "shippers/fixtures/otelcol.yaml",
	"splunk-hec": "shippers/fixtures/splunk-hec.txt",
}

func init() {
	rootCmd.AddCommand(newShippersCmd())
}

func newShippersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shippers",
		Short: "Inspect and configure log shippers",
		RunE:  runShippersList,
	}
	configCmd := &cobra.Command{
		Use:   "config <tool>",
		Short: "Print a copy-pasteable shipper config",
		Args:  cobra.ExactArgs(1),
		RunE:  runShippersConfig,
	}
	configCmd.Flags().StringVar(&shipperConfigRemote, "remote", "", "LynxDB endpoint to render into the config")
	cmd.AddCommand(configCmd)
	return cmd
}

func runShippersList(_ *cobra.Command, _ []string) error {
	ctx := context.Background()
	shippers, err := apiClient().Shippers(ctx)
	if err != nil {
		return err
	}

	if isJSONFormat() {
		b, _ := json.MarshalIndent(shippers, "", "  ")
		fmt.Println(string(b))
		return nil
	}

	if len(shippers) == 0 {
		fmt.Fprintln(os.Stdout, "No shippers observed yet.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tVERSION\tSTATUS\tLAST SEEN\tEVENTS/MIN\tENDPOINT")
	for _, s := range shippers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Tool,
			emptyDash(s.Version),
			s.Status,
			formatShipperLastSeen(s.LastSeenAt),
			formatCountHuman(s.EventsPerMin),
			s.Endpoint,
		)
	}
	return tw.Flush()
}

func runShippersConfig(_ *cobra.Command, args []string) error {
	tool := normalizeShipperTool(args[0])
	path, ok := shipperConfigTemplatePaths[tool]
	if !ok {
		return fmt.Errorf("unknown shipper %q. Use one of: filebeat, fluent-bit, vector, otelcol, splunk-hec", args[0])
	}
	tmpl, err := shipperConfigFS.ReadFile(path)
	if err != nil {
		return err
	}

	remote := shipperConfigRemote
	if remote == "" {
		remote = globalServer
	}
	out, err := renderShipperConfig(string(tmpl), shipperTemplateData{Remote: strings.TrimRight(remote, "/")})
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func renderShipperConfig(tmpl string, data shipperTemplateData) (string, error) {
	t, err := template.New("shipper").Funcs(template.FuncMap{
		"host": templateHost,
		"port": templatePort,
	}).Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func normalizeShipperTool(tool string) string {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "fluentbit", "fluent-bit":
		return "fluent-bit"
	case "otel", "otelcol", "otel-collector", "opentelemetry-collector":
		return "otelcol"
	case "splunk", "splunk-hec", "hec":
		return "splunk-hec"
	default:
		return strings.ToLower(strings.TrimSpace(tool))
	}
}

func formatShipperLastSeen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	age := time.Since(t)
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	}
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func templateHost(remote string) string {
	hostPort := strings.TrimPrefix(strings.TrimPrefix(remote, "http://"), "https://")
	if i := strings.IndexByte(hostPort, '/'); i >= 0 {
		hostPort = hostPort[:i]
	}
	if i := strings.LastIndexByte(hostPort, ':'); i >= 0 {
		return hostPort[:i]
	}
	return hostPort
}

func templatePort(remote string) string {
	hostPort := strings.TrimPrefix(strings.TrimPrefix(remote, "http://"), "https://")
	if i := strings.IndexByte(hostPort, '/'); i >= 0 {
		hostPort = hostPort[:i]
	}
	if i := strings.LastIndexByte(hostPort, ':'); i >= 0 {
		return hostPort[i+1:]
	}
	if strings.HasPrefix(remote, "https://") {
		return "443"
	}
	return "80"
}
