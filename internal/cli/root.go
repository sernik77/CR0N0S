// Package cli implements the cr0 command line.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sernik77/CR0N0S/internal/store"
)

// Version is set at build time with -ldflags "-X .../cli.Version=...".
var Version = "dev"

type globals struct {
	db     string
	system bool
	json   bool
}

var g globals

func openStore() (*store.Store, error) {
	p := g.db
	if p == "" {
		p = store.DefaultPath(g.system)
	}
	return store.Open(p)
}

// withStore opens the store for the duration of fn.
func withStore(fn func(*store.Store) error) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(st)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// NewRoot builds the command tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "cr0",
		Short: "CR0N0S - a better cron: dynamic job scheduler, tracker and templater",
		Long: `CR0N0S (cr0) schedules one-shot, cron, interval and file-watch jobs with
conditions (if/elif/else/while), hooks, reminders, templates and a full
history ledger. Jobs are executed by "cr0 daemon" (see "cr0 service").`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.db, "db", "", "database path (default $CR0_DB, ~/.local/share/cr0/cr0.db, or /var/lib/cr0/cr0.db for root)")
	pf.BoolVar(&g.system, "system", false, "use the system-wide (root) database")
	pf.BoolVar(&g.json, "json", false, "JSON output")

	root.AddCommand(newJobCmd(), newJobsCmd(), newTemplateCmd(), newDaemonCmd(),
		newServiceCmd(), newCronCmd(), newUICmd())
	return root
}

// Execute runs the CLI.
func Execute() {
	if err := NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cr0:", err)
		os.Exit(1)
	}
}

// human formats a duration compactly: 3d4h, 2h5m, 45s.
func human(d time.Duration) string {
	neg := d < 0
	if neg {
		d = -d
	}
	d = d.Round(time.Second)
	var s string
	switch {
	case d >= 24*time.Hour:
		s = fmt.Sprintf("%dd%dh", d/(24*time.Hour), (d%(24*time.Hour))/time.Hour)
	case d >= time.Hour:
		s = fmt.Sprintf("%dh%dm", d/time.Hour, (d%time.Hour)/time.Minute)
	case d >= time.Minute:
		s = fmt.Sprintf("%dm%ds", d/time.Minute, (d%time.Minute)/time.Second)
	default:
		s = fmt.Sprintf("%ds", d/time.Second)
	}
	if neg {
		return "-" + s
	}
	return s
}

func short(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
