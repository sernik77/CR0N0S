package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sernik77/CR0N0S/internal/cond"
	"github.com/sernik77/CR0N0S/internal/core"
	"github.com/sernik77/CR0N0S/internal/model"
	"github.com/sernik77/CR0N0S/internal/runner"
	"github.com/sernik77/CR0N0S/internal/sched"
	"github.com/sernik77/CR0N0S/internal/store"
)

func newJobCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "job",
		Short:   "Register, browse and manage jobs",
		Aliases: []string{"j"},
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(newRegisterCmd(), newListCmd(), newHistoryCmd(), newInspectCmd(), newModifyCmd(),
		newRescheduleCmd(), newCancelCmd(), newRunCmd(), newExportCmd())
	for _, s := range []struct{ name, short, action string }{
		{"arm", "Arm a job (enable execution)", "arm"},
		{"disarm", "Disarm a job (timer keeps running, runs are skipped)", "disarm"},
		{"pause", "Pause a job's timer", "pause"},
		{"resume", "Resume a paused job's timer", "resume"},
	} {
		c.AddCommand(newToggleCmd(s.name, s.short, s.action))
	}
	return c
}

// ---- register ----

func newRegisterCmd() *cobra.Command {
	var f jobFlags
	var tmpl string
	c := &cobra.Command{
		Use:     "register",
		Aliases: []string{"add", "new"},
		Short:   "Register a new job",
		Example: `  cr0 job register -t backup --cron "0 3 * * *" -c "restic backup ~/docs" --on-fail notify:"backup failed"
  cr0 job register -t reminder --in 25m -c 'notify-send "tea is ready"'
  cr0 job register -t build --watch src --events write -c "make" --when cmd:"git diff --quiet"
  cr0 job register --template nightly -t "nightly db dump" -p /srv/db
  cr0 job register -f job.toml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(func(st *store.Store) error {
				now := time.Now()
				j := &model.Job{}
				switch {
				case tmpl != "" && f.file != "":
					return fmt.Errorf("--template and --file are mutually exclusive")
				case tmpl != "":
					t, err := st.Template(tmpl)
					if err != nil {
						return err
					}
					j = exportSpec(&t.Spec)
					j.Template = tmpl
				case f.file != "":
					spec, err := loadSpec(f.file)
					if err != nil {
						return err
					}
					j = exportSpec(spec)
				}
				j.UUID, j.RegisteredAt = "", time.Time{}
				j.Armed = true
				if err := f.apply(cmd, j, now); err != nil {
					return err
				}
				if err := finishNew(j, now); err != nil {
					return err
				}
				if err := st.Register(j); err != nil {
					return err
				}
				if g.json {
					return printJSON(j)
				}
				fmt.Printf("registered %s  %q  next: %s\n", j.UUID, j.Title, nextText(j, now))
				return nil
			})
		},
	}
	f.bind(c.Flags(), true)
	c.Flags().StringVarP(&tmpl, "template", "T", "", "start from a template; flags override its fields")
	return c
}

// finishNew applies defaults and checks that need the clock or the host.
func finishNew(j *model.Job, now time.Time) error {
	if j.Path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		j.Path = wd
	}
	if j.Title == "" {
		return fmt.Errorf("--title is required")
	}
	if j.Schedule.Kind == "" {
		return fmt.Errorf("a schedule is required: --at, --in, --cron, --every, --watch or --manual")
	}
	if s := &j.Schedule; s.Kind == model.ScheduleOnce && s.At.IsZero() && s.In > 0 {
		s.At = now.Add(s.In.D()).Truncate(time.Second)
	}
	if j.Schedule.Kind == model.ScheduleOnce && !j.Schedule.At.After(now) {
		return fmt.Errorf("execution time %s is in the past", j.Schedule.At.Format(time.RFC3339))
	}
	if err := checkSuperuser(j); err != nil {
		return err
	}
	j.RegisteredAt = now.Truncate(time.Second)
	n, err := sched.Next(j, now)
	j.NextRun = n
	return err
}

func checkSuperuser(j *model.Job) error {
	if j.Superuser && j.SudoMode == model.SudoPreauth && os.Geteuid() != 0 {
		return fmt.Errorf("pre-authorized superuser jobs live in the system scope; authorize now with:\n  pkexec cr0 --system job register ...\n(or use --sudo-mode exec to be asked by pkexec at execution time)")
	}
	return nil
}

func nextText(j *model.Job, now time.Time) string {
	switch {
	case j.Paused:
		return "paused"
	case j.NextRun != nil:
		return fmt.Sprintf("%s (in %s)", fmtTime(j.NextRun), human(j.NextRun.Sub(now)))
	case j.Schedule.Kind == model.ScheduleWatch:
		return "on change of " + strings.Join(j.Schedule.Watch, ", ")
	case j.Schedule.Kind == model.ScheduleManual:
		return "manual"
	}
	return "-"
}

// ---- list ----

type listFilter struct {
	all, here, past, future bool
	path, command, status   string
	tag                     string
	limit                   int
}

func (lf *listFilter) bind(c *cobra.Command) {
	fs := c.Flags()
	fs.BoolVarP(&lf.all, "all", "a", false, "include done and cancelled jobs")
	fs.BoolVar(&lf.here, "here", false, "only jobs whose path is the current directory")
	fs.StringVar(&lf.path, "path", "", "only jobs at or below this path")
	fs.StringVar(&lf.command, "command", "", "only jobs whose command/script contains this text")
	fs.BoolVar(&lf.past, "past", false, "only jobs that have already run / are finished")
	fs.BoolVar(&lf.future, "future", false, "only jobs with an upcoming execution")
	fs.StringVar(&lf.status, "status", "", "only jobs with this status (active, done, cancelled) or last run status")
	fs.StringVar(&lf.tag, "tag", "", "only jobs with this tag")
	fs.IntVarP(&lf.limit, "limit", "n", 0, "maximum number of jobs")
}

func (lf *listFilter) match(j *model.Job, now time.Time) bool {
	if !lf.all && lf.status == "" && !lf.past && j.Status != model.StatusActive {
		return false
	}
	if lf.here {
		wd, _ := os.Getwd()
		if j.Path != wd {
			return false
		}
	}
	if lf.path != "" {
		p, _ := filepath.Abs(lf.path)
		if j.Path != p && !strings.HasPrefix(j.Path, p+"/") {
			return false
		}
	}
	if lf.command != "" && !strings.Contains(j.Command+j.Script, lf.command) {
		return false
	}
	if lf.status != "" && j.Status != lf.status && j.LastStatus != lf.status {
		return false
	}
	if lf.tag != "" && !contains(j.Tags, lf.tag) {
		return false
	}
	if lf.future && (j.NextRun == nil || !j.NextRun.After(now) || j.Status != model.StatusActive) {
		return false
	}
	if lf.past && j.LastRun == nil && j.Status == model.StatusActive {
		return false
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func filterJobs(st *store.Store, lf *listFilter, now time.Time) ([]*model.Job, error) {
	jobs, err := st.List()
	if err != nil {
		return nil, err
	}
	var out []*model.Job
	for _, j := range jobs {
		if lf.match(j, now) {
			out = append(out, j)
			if lf.limit > 0 && len(out) == lf.limit {
				break
			}
		}
	}
	return out, nil
}

func scheduleText(s model.Schedule) string {
	switch s.Kind {
	case model.ScheduleOnce:
		return "once"
	case model.ScheduleCron:
		return "cron " + s.Cron
	case model.ScheduleInterval:
		return "every " + s.Every.String()
	case model.ScheduleWatch:
		return "watch"
	}
	return s.Kind
}

func stateText(j *model.Job) string {
	if j.Status != model.StatusActive {
		return j.Status
	}
	var parts []string
	if !j.Armed {
		parts = append(parts, "disarmed")
	}
	if j.Paused {
		parts = append(parts, "paused")
	}
	if len(parts) == 0 {
		return "armed"
	}
	return strings.Join(parts, ",")
}

func condText(ins core.Inspection) string {
	if ins.Count == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d %v", ins.MetCount, ins.Count, ins.Met)
}

// JobRow is the JSON shape of a listed job.
type JobRow struct {
	*model.Job
	Conditions core.Inspection `json:"conditions_summary"`
}

func newListCmd() *cobra.Command {
	var lf listFilter
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List jobs with schedule, state and conditions met",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(func(st *store.Store) error {
				now := time.Now()
				jobs, err := filterJobs(st, &lf, now)
				if err != nil {
					return err
				}
				if g.json {
					rows := make([]JobRow, 0, len(jobs))
					for _, j := range jobs {
						rows = append(rows, JobRow{j, core.Inspect(st, j, now)})
					}
					return printJSON(rows)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tTITLE\tSCHEDULE\tNEXT\tSTATE\tLAST\tCOND (met/n met)\tPATH")
				for _, j := range jobs {
					last := j.LastStatus
					if last == "" {
						last = "-"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.UUID[:8], short(j.Title, 28), short(scheduleText(j.Schedule), 22),
						nextText(j, now), stateText(j), last, condText(core.Inspect(st, j, now)), j.Path)
				}
				return tw.Flush()
			})
		},
	}
	lf.bind(c)
	return c
}

// ---- jobs (quick upcoming list) ----

func newJobsCmd() *cobra.Command {
	var lf listFilter
	c := &cobra.Command{
		Use:   "jobs",
		Short: "Quick list of upcoming jobs: what, where, when",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(func(st *store.Store) error {
				now := time.Now()
				jobs, err := filterJobs(st, &lf, now)
				if err != nil {
					return err
				}
				if g.json {
					return printJSON(jobs)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tWHAT\tWHERE\tWHEN\tUNTIL\tREGISTERED\tSTATE")
				for _, j := range jobs {
					until := "-"
					if j.NextRun != nil && !j.Paused {
						until = human(j.NextRun.Sub(now))
					}
					what := j.Title
					if c := j.Command + j.Script; c != "" {
						what += ": " + c
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s ago\t%s\n", j.UUID[:8], short(what, 40), short(j.Path, 30),
						short(nextText(j, now), 30), until, human(now.Sub(j.RegisteredAt)), stateText(j))
				}
				return tw.Flush()
			})
		},
	}
	lf.bind(c)
	return c
}

// ---- history ----

func newHistoryCmd() *cobra.Command {
	var limit int
	var status string
	var revisions bool
	c := &cobra.Command{
		Use:     "history [job]",
		Aliases: []string{"log"},
		Short:   "Show the run ledger (and audit trail with --revisions)",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(st *store.Store) error {
				id := ""
				if len(args) == 1 {
					u, err := st.Resolve(args[0])
					if err != nil {
						return err
					}
					id = u
				}
				if revisions {
					revs, err := st.Revisions(id, limit)
					if err != nil {
						return err
					}
					if g.json {
						return printJSON(revs)
					}
					tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
					fmt.Fprintln(tw, "AT\tJOB\tACTION\tACTOR\tREASON\tDETAILS")
					for _, r := range revs {
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", fmtTime(&r.At), r.JobUUID[:8], r.Action, r.Actor, short(r.Reason, 40), short(r.ReasonDesc, 50))
					}
					return tw.Flush()
				}
				runs, err := st.Runs(id, 0)
				if err != nil {
					return err
				}
				var out []model.Run
				for _, r := range runs {
					if status == "" || r.Status == status {
						out = append(out, r)
						if limit > 0 && len(out) == limit {
							break
						}
					}
				}
				if g.json {
					return printJSON(out)
				}
				titles := map[string]string{}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "RUN\tSTARTED\tJOB\tTITLE\tTRIGGER\tSTATUS\tEXIT\tDURATION\tNOTE")
				for _, r := range out {
					if _, ok := titles[r.JobUUID]; !ok {
						if j, err := st.Get(r.JobUUID); err == nil {
							titles[r.JobUUID] = j.Title
						}
					}
					fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", r.ID, fmtTime(&r.StartedAt), r.JobUUID[:8], short(titles[r.JobUUID], 24),
						r.Trigger, r.Status, r.ExitCode, human(r.FinishedAt.Sub(r.StartedAt)), short(r.Error, 50))
				}
				return tw.Flush()
			})
		},
	}
	c.Flags().IntVarP(&limit, "limit", "n", 50, "maximum number of entries (0 = all)")
	c.Flags().StringVar(&status, "status", "", "only runs with this status (success, fail, error, unknown, skipped, missed)")
	c.Flags().BoolVarP(&revisions, "revisions", "r", false, "show the audit trail (registrations, edits with reasons, state changes)")
	return c
}

// ---- inspect ----

func newInspectCmd() *cobra.Command {
	var output bool
	c := &cobra.Command{
		Use:     "inspect <job>",
		Aliases: []string{"show", "info"},
		Short:   "Show a job in detail, including whether its conditions are met",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(st *store.Store) error {
				j, err := st.Get(args[0])
				if err != nil {
					return err
				}
				now := time.Now()
				ins := core.Inspect(st, j, now)
				runs, _ := st.Runs(j.UUID, 5)
				revs, _ := st.Revisions(j.UUID, 5)
				if g.json {
					return printJSON(map[string]any{"job": j, "conditions": ins, "runs": runs, "revisions": revs})
				}
				printInspect(j, ins, runs, revs, now, output)
				return nil
			})
		},
	}
	c.Flags().BoolVarP(&output, "output", "o", false, "include the output of the last run")
	return c
}

func printInspect(j *model.Job, ins core.Inspection, runs []model.Run, revs []model.Revision, now time.Time, output bool) {
	kv := func(k, v string) {
		if v != "" {
			fmt.Printf("  %-14s %s\n", k+":", v)
		}
	}
	fmt.Printf("%s  %s\n", j.UUID, j.Title)
	kv("description", j.Description)
	kv("status", j.Status+" / "+stateText(j))
	kv("schedule", scheduleText(j.Schedule))
	kv("next run", nextText(j, now))
	kv("registered", fmt.Sprintf("%s (%s ago)", fmtTime(&j.RegisteredAt), human(now.Sub(j.RegisteredAt))))
	if j.LastRun != nil {
		kv("last run", fmt.Sprintf("%s  %s", fmtTime(j.LastRun), j.LastStatus))
	}
	kv("path", j.Path)
	kv("user", j.User)
	if j.Superuser {
		kv("superuser", "yes ("+j.SudoMode+")")
	}
	kv("interpreter", j.Interpreter)
	kv("command", j.Command)
	kv("script", j.Script)
	if len(j.Args) > 0 {
		kv("args", strings.Join(j.Args, " "))
	}
	for k, v := range j.Env {
		kv("env", k+"="+v)
	}
	kv("timeout", j.Timeout.String())
	kv("remind", j.RemindBefore.String())
	kv("catchup", j.Catchup)
	kv("template", j.Template)
	if len(j.Tags) > 0 {
		kv("tags", strings.Join(j.Tags, ", "))
	}
	if n := len(j.Logic); n > 0 {
		kv("logic", fmt.Sprintf("%d top-level steps (see 'cr0 job export')", n))
	}
	if n := j.Hooks.Count(); n > 0 {
		kv("hooks", fmt.Sprintf("%d (success %d, fail %d, error %d, unknown %d)", n,
			len(j.Hooks.OnSuccess), len(j.Hooks.OnFail), len(j.Hooks.OnError), len(j.Hooks.OnUnknown)))
	}
	fmt.Printf("\nconditions: %d, met %d, conditions met: %v (evaluated for %s)\n", ins.Count, ins.MetCount, ins.Met, fmtTime(&ins.EvaluatedAt))
	if ins.Tree != nil {
		printTree(*ins.Tree, "  ")
	}
	for id, met := range ins.Dependencies {
		fmt.Printf("  depends on job %s: its own conditions met: %v\n", id, met)
	}
	if len(runs) > 0 {
		fmt.Println("\nlast runs:")
		for _, r := range runs {
			fmt.Printf("  #%d %s  %-8s %-7s exit=%d %s\n", r.ID, fmtTime(&r.StartedAt), r.Trigger, r.Status, r.ExitCode, r.Error)
		}
		if output && runs[0].Output != "" {
			fmt.Printf("\noutput of run #%d:\n%s\n", runs[0].ID, strings.TrimRight(runs[0].Output, "\n"))
		}
	}
	if len(revs) > 0 {
		fmt.Println("\nrevisions:")
		for _, r := range revs {
			line := fmt.Sprintf("  %s  %-10s by %s: %s", fmtTime(&r.At), r.Action, r.Actor, r.Reason)
			if r.ReasonDesc != "" {
				line += " - " + r.ReasonDesc
			}
			fmt.Println(line)
		}
	}
}

func printTree(r cond.Result, indent string) {
	mark := "✗"
	if r.Met {
		mark = "✓"
	}
	line := fmt.Sprintf("%s%s %s", indent, mark, r.Desc)
	if r.Err != "" {
		line += "  (" + r.Err + ")"
	}
	fmt.Println(line)
	for _, c := range r.Children {
		printTree(c, indent+"  ")
	}
}

// ---- modify / reschedule ----

type reasonFlags struct{ reason, desc string }

func (r *reasonFlags) bind(c *cobra.Command, required bool) {
	help := "short reason for the change"
	if required {
		help += " (required)"
	}
	c.Flags().StringVarP(&r.reason, "reason", "r", "", help)
	c.Flags().StringVar(&r.desc, "reason-desc", "", "longer explanation of the change")
}

func newModifyCmd() *cobra.Command {
	var f jobFlags
	var rf reasonFlags
	c := &cobra.Command{
		Use:     "modify <job> --reason TEXT [flags]",
		Aliases: []string{"edit", "mod"},
		Short:   "Modify a job (a reason is mandatory)",
		Example: `  cr0 job modify 3fa8 -r "slower disk" --timeout 2h
  cr0 job modify 3fa8 -r "new spec" --reason-desc "moved to restic 0.17 flags" -f job.toml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(st *store.Store) error {
				now := time.Now()
				j, err := st.Update(args[0], store.Audit{Action: "modify", Reason: rf.reason, ReasonDesc: rf.desc}, func(j *model.Job) error {
					if f.file != "" {
						spec, err := loadSpec(f.file)
						if err != nil {
							return err
						}
						keep := j.Clone()
						*j = *exportSpec(spec)
						j.Status, j.Armed, j.Paused, j.PausedAt = keep.Status, keep.Armed, keep.Paused, keep.PausedAt
						j.LastRun, j.LastStatus, j.Template = keep.LastRun, keep.LastStatus, keep.Template
					}
					if err := f.apply(cmd, j, now); err != nil {
						return err
					}
					if err := checkSuperuser(j); err != nil {
						return err
					}
					return reschedule(j, now, f.file != "" || scheduleChanged(cmd))
				})
				if err != nil {
					return err
				}
				fmt.Printf("modified %s  %q  next: %s\n", j.UUID, j.Title, nextText(j, now))
				return nil
			})
		},
	}
	f.bind(c.Flags(), true)
	rf.bind(c, true)
	return c
}

func reschedule(j *model.Job, now time.Time, scheduleChanged bool) error {
	if !scheduleChanged {
		return nil
	}
	if j.Schedule.Kind == model.ScheduleOnce {
		if j.Schedule.At.IsZero() && j.Schedule.In > 0 {
			j.Schedule.At = now.Add(j.Schedule.In.D()).Truncate(time.Second)
		}
		if !j.Schedule.At.After(now) {
			return fmt.Errorf("execution time is in the past")
		}
		j.LastRun = nil
		if j.Status == model.StatusDone {
			j.Status = model.StatusActive
		}
	}
	n, err := sched.Next(j, now)
	j.NextRun = n
	return err
}

func newRescheduleCmd() *cobra.Command {
	var f jobFlags
	var rf reasonFlags
	c := &cobra.Command{
		Use:   "reschedule <job> --reason TEXT (--at|--in|--cron|--every|--watch|--manual)",
		Short: "Change when a job runs (a reason is mandatory)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !scheduleChanged(cmd) {
				return fmt.Errorf("give a new schedule: --at, --in, --cron, --every, --watch or --manual")
			}
			return withStore(func(st *store.Store) error {
				now := time.Now()
				j, err := st.Update(args[0], store.Audit{Action: "reschedule", Reason: rf.reason, ReasonDesc: rf.desc}, func(j *model.Job) error {
					s, err := f.schedule(cmd, now)
					if err != nil {
						return err
					}
					j.Schedule = s
					return reschedule(j, now, true)
				})
				if err != nil {
					return err
				}
				fmt.Printf("rescheduled %s  next: %s\n", j.UUID[:8], nextText(j, now))
				return nil
			})
		},
	}
	f.bindSchedule(c.Flags())
	rf.bind(c, true)
	return c
}

// ---- cancel / toggles ----

func newCancelCmd() *cobra.Command {
	var rf reasonFlags
	c := &cobra.Command{
		Use:     "cancel <job>...",
		Aliases: []string{"rm"},
		Short:   "Cancel jobs (they stay in the history)",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(st *store.Store) error {
				for _, id := range args {
					j, err := st.Update(id, store.Audit{Action: "cancel", Reason: rf.reason, ReasonDesc: rf.desc}, func(j *model.Job) error {
						j.Status, j.NextRun = model.StatusCancelled, nil
						return nil
					})
					if err != nil {
						return err
					}
					fmt.Printf("cancelled %s  %q\n", j.UUID[:8], j.Title)
				}
				return nil
			})
		},
	}
	rf.bind(c, false)
	return c
}

func newToggleCmd(name, short, action string) *cobra.Command {
	var rf reasonFlags
	c := &cobra.Command{
		Use:   name + " <job>...",
		Short: short,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(st *store.Store) error {
				now := time.Now()
				for _, id := range args {
					j, err := st.Update(id, store.Audit{Action: action, Reason: rf.reason, ReasonDesc: rf.desc}, func(j *model.Job) error {
						return Toggle(j, action, now)
					})
					if err != nil {
						return err
					}
					fmt.Printf("%s %s  %q  state: %s  next: %s\n", action, j.UUID[:8], j.Title, stateText(j), nextText(j, now))
				}
				return nil
			})
		},
	}
	rf.bind(c, false)
	return c
}

// Toggle applies arm/disarm/pause/resume to a job.
func Toggle(j *model.Job, action string, now time.Time) error {
	if j.Status != model.StatusActive {
		return fmt.Errorf("job is %s", j.Status)
	}
	switch action {
	case "arm":
		j.Armed = true
	case "disarm":
		j.Armed = false
	case "pause":
		sched.Pause(j, now)
	case "resume":
		return sched.Resume(j, now)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return nil
}

// ---- run / export ----

func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <job>",
		Short: "Run a job now in the foreground (conditions still apply)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(st *store.Store) error {
				j, err := st.Get(args[0])
				if err != nil {
					return err
				}
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
				defer stop()
				run, err := core.Execute(ctx, st, runner.New(), j, core.TriggerManual)
				if err != nil {
					return err
				}
				if g.json {
					return printJSON(run)
				}
				fmt.Print(run.Output)
				fmt.Fprintf(os.Stderr, "cr0: run #%d %s exit=%d %s\n", run.ID, run.Status, run.ExitCode, run.Error)
				if run.Status != model.RunSuccess {
					os.Exit(2)
				}
				return nil
			})
		},
	}
}

func newExportCmd() *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "export <job>",
		Short: "Print a job's spec as TOML or JSON (for 'register -f' or templates)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(st *store.Store) error {
				j, err := st.Get(args[0])
				if err != nil {
					return err
				}
				s, err := encodeSpec(exportSpec(j), format)
				if err != nil {
					return err
				}
				fmt.Print(s)
				return nil
			})
		},
	}
	c.Flags().StringVar(&format, "format", "toml", "toml or json")
	return c
}
