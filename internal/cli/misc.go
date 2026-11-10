package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sernik77/CR0N0S/internal/daemon"
	"github.com/sernik77/CR0N0S/internal/model"
	"github.com/sernik77/CR0N0S/internal/runner"
	"github.com/sernik77/CR0N0S/internal/sched"
	"github.com/sernik77/CR0N0S/internal/store"
	"github.com/sernik77/CR0N0S/internal/tui"
)

// ---- template ----

func newTemplateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "template",
		Aliases: []string{"tpl"},
		Short:   "Manage job templates (use with 'job register --template NAME')",
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	var f jobFlags
	var from, tdesc string
	add := &cobra.Command{
		Use:   "add <name> [job flags]",
		Short: "Create or replace a template from flags, a spec file (-f) or an existing job (--from)",
		Example: `  cr0 template add nightly --cron "0 2 * * *" --shell bash --timeout 1h --on-fail notify:"{title} failed"
  cr0 template add deploy --from 3fa8`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(st *store.Store) error {
				spec := &model.Job{}
				switch {
				case from != "":
					j, err := st.Get(from)
					if err != nil {
						return err
					}
					spec = exportSpec(j)
				case f.file != "":
					j, err := loadSpec(f.file)
					if err != nil {
						return err
					}
					spec = exportSpec(j)
				}
				spec.UUID, spec.RegisteredAt, spec.Armed = "", time.Time{}, true
				if err := f.apply(cmd, spec, time.Now()); err != nil {
					return err
				}
				spec = exportSpec(spec)
				if err := st.SaveTemplate(&model.Template{Name: args[0], Description: tdesc, Spec: *spec}); err != nil {
					return err
				}
				fmt.Printf("template %q saved\n", args[0])
				return nil
			})
		},
	}
	f.bind(add.Flags(), true)
	add.Flags().StringVar(&from, "from", "", "copy the definition of an existing job")
	add.Flags().StringVar(&tdesc, "template-desc", "", "description of the template itself")

	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List templates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(func(st *store.Store) error {
				ts, err := st.Templates()
				if err != nil {
					return err
				}
				if g.json {
					return printJSON(ts)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tSCHEDULE\tCOMMAND\tDESCRIPTION")
				for _, t := range ts {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, scheduleText(t.Spec.Schedule), short(t.Spec.Command+t.Spec.Script, 40), short(t.Description, 40))
				}
				return tw.Flush()
			})
		},
	}

	var format string
	show := &cobra.Command{
		Use:   "show <name>",
		Short: "Print a template spec",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(st *store.Store) error {
				t, err := st.Template(args[0])
				if err != nil {
					return err
				}
				s, err := encodeSpec(&t.Spec, format)
				if err == nil {
					fmt.Print(s)
				}
				return err
			})
		},
	}
	show.Flags().StringVar(&format, "format", "toml", "toml or json")

	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a template",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(st *store.Store) error { return st.DeleteTemplate(args[0]) })
		},
	}
	c.AddCommand(add, list, show, rm)
	return c
}

// ---- daemon ----

func newDaemonCmd() *cobra.Command {
	var once bool
	var poll time.Duration
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Run the scheduler (normally started by systemd, see 'cr0 service')",
		Long: `Run the scheduler loop. With --once, perform a single scheduling pass,
wait for started jobs and exit; this lets cronie drive cr0 instead of systemd:
  * * * * * cr0 daemon --once`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(func(st *store.Store) error {
				d := daemon.New(st, runner.New(), log.New(os.Stderr, "", log.LstdFlags))
				d.Poll = poll
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				if once {
					d.Tick(ctx, time.Now())
					d.Wait()
					return nil
				}
				return d.Run(ctx)
			})
		},
	}
	c.Flags().BoolVar(&once, "once", false, "single scheduling pass, then exit")
	c.Flags().DurationVar(&poll, "poll", time.Second, "scheduling interval")
	return c
}

// ---- service ----

const unitTemplate = `[Unit]
Description=CR0N0S job scheduler (%s)
Documentation=https://github.com/sernik77/CR0N0S
After=%s

[Service]
Type=simple
ExecStart=%s%s daemon
Restart=on-failure
RestartSec=5

[Install]
WantedBy=%s
`

// SystemBinary is the root-owned binary the system unit runs. It must not be
// a user-writable path, or any process of that user could gain root.
const SystemBinary = "/usr/local/bin/cr0"

// Unit renders the systemd unit for the user or system daemon.
func Unit(bin string, system bool) string {
	if system {
		return fmt.Sprintf(unitTemplate, "system", "network-online.target", SystemBinary, " --system", "multi-user.target")
	}
	return fmt.Sprintf(unitTemplate, "user", "default.target", bin, "", "default.target")
}

func newServiceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "service",
		Short: "systemd integration for the cr0 daemon",
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	binary := func() string {
		if p, err := os.Executable(); err == nil {
			return p
		}
		return "/usr/local/bin/cr0"
	}
	unit := &cobra.Command{
		Use:   "unit",
		Short: "Print the systemd unit (--system for the root daemon)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Print(Unit(binary(), g.system))
			return nil
		},
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install the user unit (or print the steps for the system unit with --system)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.system {
				fmt.Printf("Install the system unit (runs as root, executes pre-authorized superuser jobs and jobs for other users).\n"+
					"The unit runs the root-owned %s, never a user-writable copy:\n\n"+
					"  pkexec install -Dm755 -o root -g root %s %s\n"+
					"  cr0 service unit --system | pkexec tee /etc/systemd/system/cr0.service >/dev/null\n"+
					"  pkexec systemctl daemon-reload\n  pkexec systemctl enable --now cr0.service\n\nUnit:\n\n%s",
					SystemBinary, binary(), SystemBinary, Unit(binary(), true))
				return nil
			}
			cfg, err := os.UserConfigDir()
			if err != nil {
				return err
			}
			path := filepath.Join(cfg, "systemd", "user", "cr0.service")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(Unit(binary(), false)), 0o644); err != nil {
				return err
			}
			fmt.Printf("wrote %s\nenable it with:\n  systemctl --user daemon-reload && systemctl --user enable --now cr0.service\n"+
				"(run 'loginctl enable-linger $USER' to keep it running while logged out)\n", path)
			return nil
		},
	}
	c.AddCommand(unit, install)
	return c
}

// ---- cron (cronie integration) ----

func newCronCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cron",
		Short: "cronie integration: import/export crontab entries",
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	var file string
	var dry bool
	imp := &cobra.Command{
		Use:   "import",
		Short: "Import crontab entries as cr0 jobs (default: current user's 'crontab -l')",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var r io.Reader
			if file == "" || file == "-" {
				if file == "-" {
					r = os.Stdin
				} else {
					out, err := exec.Command("crontab", "-l").Output()
					if err != nil {
						return fmt.Errorf("crontab -l: %w", err)
					}
					r = strings.NewReader(string(out))
				}
			} else {
				fh, err := os.Open(file)
				if err != nil {
					return err
				}
				defer fh.Close()
				r = fh
			}
			jobs, warnings := ParseCrontab(r)
			for _, w := range warnings {
				fmt.Fprintln(os.Stderr, "skip:", w)
			}
			if dry {
				for _, j := range jobs {
					fmt.Printf("%-20s %s\n", j.Schedule.Cron, j.Command)
				}
				return nil
			}
			return withStore(func(st *store.Store) error {
				now := time.Now()
				for _, j := range jobs {
					n, err := sched.Next(j, now)
					if err != nil {
						return err
					}
					j.NextRun = n
					if err := st.Register(j); err != nil {
						return err
					}
					fmt.Printf("imported %s  %s  %s\n", j.UUID[:8], j.Schedule.Cron, short(j.Command, 60))
				}
				fmt.Println("note: remove the imported lines from your crontab (crontab -e) to avoid double execution")
				return nil
			})
		},
	}
	imp.Flags().StringVarP(&file, "file", "f", "", "crontab file to read ('-' for stdin)")
	imp.Flags().BoolVar(&dry, "dry-run", false, "only show what would be imported")

	var tick bool
	exp := &cobra.Command{
		Use:   "export",
		Short: "Print cron jobs as crontab lines (or, with --tick, the line that lets cronie drive the cr0 daemon)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if tick {
				bin, _ := os.Executable()
				fmt.Printf("* * * * * %s daemon --once\n", bin)
				return nil
			}
			return withStore(func(st *store.Store) error {
				jobs, err := st.Active()
				if err != nil {
					return err
				}
				for _, j := range jobs {
					if j.Schedule.Kind != model.ScheduleCron || j.Command == "" {
						continue
					}
					fmt.Printf("%s cd %s && %s  # cr0:%s %s\n", j.Schedule.Cron, shellQuote(j.Path), j.Command, j.UUID, j.Title)
				}
				return nil
			})
		},
	}
	exp.Flags().BoolVar(&tick, "tick", false, "print '* * * * * cr0 daemon --once' for cronie-driven scheduling")
	c.AddCommand(imp, exp)
	return c
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var envAssign = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$`)

// ParseCrontab converts crontab lines to jobs. Variable assignments are
// carried into each following job's environment.
func ParseCrontab(r io.Reader) ([]*model.Job, []string) {
	home, _ := os.UserHomeDir()
	env := map[string]string{}
	var jobs []*model.Job
	var warnings []string
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := envAssign.FindStringSubmatch(line); m != nil {
			env[m[1]] = strings.Trim(strings.TrimSpace(m[2]), `"'`)
			continue
		}
		fields := strings.Fields(line)
		var expr, command string
		if strings.HasPrefix(fields[0], "@") {
			if fields[0] == "@reboot" {
				warnings = append(warnings, fmt.Sprintf("line %d: @reboot is not supported", n))
				continue
			}
			expr, command = fields[0], strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		} else if len(fields) >= 6 {
			expr = strings.Join(fields[:5], " ")
			rest := line
			for i := 0; i < 5; i++ {
				rest = strings.TrimSpace(rest)
				rest = rest[strings.IndexAny(rest, " \t"):]
			}
			command = strings.TrimSpace(rest)
		} else {
			warnings = append(warnings, fmt.Sprintf("line %d: cannot parse %q", n, line))
			continue
		}
		if _, err := sched.ParseCron(expr); err != nil {
			warnings = append(warnings, fmt.Sprintf("line %d: %v", n, err))
			continue
		}
		j := &model.Job{
			Title: "cron: " + short(command, 40), Path: home, Command: command, Interpreter: "sh",
			Schedule: model.Schedule{Kind: model.ScheduleCron, Cron: expr}, Armed: true, Tags: []string{"cron-import"},
		}
		if len(env) > 0 {
			j.Env = map[string]string{}
			for k, v := range env {
				if k != "SHELL" && k != "MAILTO" {
					j.Env[k] = v
				}
			}
			if sh, ok := env["SHELL"]; ok {
				j.Interpreter = sh
			}
		}
		jobs = append(jobs, j)
	}
	return jobs, warnings
}

// ---- ui ----

func newUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ui",
		Short: "Interactive TUI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(func(st *store.Store) error { return tui.Run(st, Toggle) })
		},
	}
}
