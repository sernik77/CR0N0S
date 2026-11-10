package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sernik77/CR0N0S/internal/cond"
	"github.com/sernik77/CR0N0S/internal/model"
	"github.com/sernik77/CR0N0S/internal/sched"
)

// jobFlags are the job-definition flags shared by register, modify and
// template add. Only flags the user actually set are applied.
type jobFlags struct {
	title, desc                          string
	at, in, cron, every                  string
	watch, events                        []string
	debounce                             string
	manual                               bool
	user, path                           string
	sudo                                 bool
	sudoMode                             string
	command, script, shell               string
	args, env                            []string
	timeout, remind, catchup             string
	when, whenAny                        []string
	noConditions                         bool
	onSuccess, onFail, onError, onUnknown []string
	tags                                 []string
	file                                 string
	disarmed                             bool
}

func (f *jobFlags) bind(fs *pflag.FlagSet, withSpec bool) {
	fs.StringVarP(&f.title, "title", "t", "", "short title")
	fs.StringVarP(&f.desc, "desc", "d", "", "description")
	f.bindSchedule(fs)
	fs.StringVarP(&f.user, "user", "u", "", "run as this user (needs the system daemon when not yourself)")
	fs.StringVarP(&f.path, "path", "p", "", "absolute working directory (default: current directory)")
	fs.BoolVar(&f.sudo, "sudo", false, "run as superuser")
	fs.StringVar(&f.sudoMode, "sudo-mode", model.SudoExec, "superuser authorization: preauth (system daemon) or exec (pkexec at run time)")
	fs.StringVarP(&f.command, "command", "c", "", "one-line command")
	fs.StringVarP(&f.script, "script", "s", "", "script path (absolute or relative to --path)")
	fs.StringVar(&f.shell, "shell", "", "interpreter: sh bash zsh fish pwsh python node php ruby perl (default sh)")
	fs.StringArrayVar(&f.args, "arg", nil, "argument passed to the command/script (repeatable)")
	fs.StringArrayVarP(&f.env, "env", "e", nil, "environment variable KEY=VALUE (repeatable)")
	fs.StringVar(&f.timeout, "timeout", "", "kill the job after this duration (e.g. 10m)")
	fs.StringVar(&f.remind, "remind", "", "desktop reminder this long before execution (e.g. 15m)")
	fs.StringVar(&f.catchup, "catchup", "", "missed-run policy: once (default) or skip")
	fs.StringArrayVarP(&f.when, "when", "w", nil, "condition, all must hold (repeatable): file:P dir:P changed:P cmd:C env:K[=V] time:HH:MM-HH:MM days:mon,fri job:ID[=status] proc:NAME, prefix ! to negate")
	fs.StringArrayVar(&f.whenAny, "when-any", nil, "condition, at least one must hold (repeatable, same syntax as --when)")
	fs.BoolVar(&f.noConditions, "no-conditions", false, "remove all conditions")
	fs.StringArrayVar(&f.onSuccess, "on-success", nil, "hook on success: notify:MSG | webhook:URL | cmd:COMMAND (repeatable)")
	fs.StringArrayVar(&f.onFail, "on-fail", nil, "hook on non-zero exit (repeatable)")
	fs.StringArrayVar(&f.onError, "on-error", nil, "hook when the job could not run or timed out (repeatable)")
	fs.StringArrayVar(&f.onUnknown, "on-unknown", nil, "hook when the job was killed by a signal (repeatable)")
	fs.StringSliceVar(&f.tags, "tag", nil, "tags (comma separated)")
	fs.BoolVar(&f.disarmed, "disarmed", false, "register the job disarmed (it will not execute until armed)")
	if withSpec {
		fs.StringVarP(&f.file, "file", "f", "", "job spec file (.toml or .json); flags override its fields")
	}
}

func (f *jobFlags) bindSchedule(fs *pflag.FlagSet) {
	fs.StringVar(&f.at, "at", "", `run once at a time: "2026-12-24 18:00", "18:00" or RFC3339`)
	fs.StringVar(&f.in, "in", "", "run once after a delay (e.g. 30m, 2h, 1d)")
	fs.StringVar(&f.cron, "cron", "", `cron expression ("*/5 * * * *", 6 fields with seconds, or @daily)`)
	fs.StringVar(&f.every, "every", "", "repeat at a fixed interval (e.g. 15m)")
	fs.StringArrayVar(&f.watch, "watch", nil, "run when this path changes (inotify, repeatable)")
	fs.StringSliceVar(&f.events, "events", nil, "watch events: create,write,remove,rename,chmod (default all)")
	fs.StringVar(&f.debounce, "debounce", "", "quiet period after a watched change (default 2s)")
	fs.BoolVar(&f.manual, "manual", false, "no schedule; run only with 'cr0 job run'")
}

// parseAt parses absolute times. A bare "HH:MM" means the next occurrence.
func parseAt(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	for _, layout := range []string{"15:04:05", "15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local)
			if !at.After(now) {
				at = at.AddDate(0, 0, 1)
			}
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q", s)
}

func changed(cmd *cobra.Command, names ...string) bool {
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

// scheduleChanged reports whether any schedule flag was set.
func scheduleChanged(cmd *cobra.Command) bool {
	return changed(cmd, "at", "in", "cron", "every", "watch", "manual")
}

func (f *jobFlags) schedule(cmd *cobra.Command, now time.Time) (model.Schedule, error) {
	n := 0
	for _, name := range []string{"at", "in", "cron", "every", "watch", "manual"} {
		if cmd.Flags().Changed(name) {
			n++
		}
	}
	if n > 1 {
		return model.Schedule{}, fmt.Errorf("--at, --in, --cron, --every, --watch and --manual are mutually exclusive")
	}
	var s model.Schedule
	switch {
	case changed(cmd, "at"):
		t, err := parseAt(f.at, now)
		if err != nil {
			return s, err
		}
		s = model.Schedule{Kind: model.ScheduleOnce, At: t}
	case changed(cmd, "in"):
		d, err := model.ParseDuration(f.in)
		if err != nil {
			return s, err
		}
		s = model.Schedule{Kind: model.ScheduleOnce, At: now.Add(d).Truncate(time.Second), In: model.Duration(d)}
	case changed(cmd, "cron"):
		if _, err := sched.ParseCron(f.cron); err != nil {
			return s, err
		}
		s = model.Schedule{Kind: model.ScheduleCron, Cron: f.cron}
	case changed(cmd, "every"):
		d, err := model.ParseDuration(f.every)
		if err != nil {
			return s, err
		}
		s = model.Schedule{Kind: model.ScheduleInterval, Every: model.Duration(d)}
	case changed(cmd, "watch"):
		s = model.Schedule{Kind: model.ScheduleWatch, Watch: f.watch, Events: f.events}
		if f.debounce != "" {
			d, err := model.ParseDuration(f.debounce)
			if err != nil {
				return s, err
			}
			s.Debounce = model.Duration(d)
		}
	case changed(cmd, "manual"):
		s = model.Schedule{Kind: model.ScheduleManual}
	}
	return s, nil
}

func parseDur(s string) (model.Duration, error) {
	d, err := model.ParseDuration(s)
	return model.Duration(d), err
}

// apply copies the flags that were set onto j.
func (f *jobFlags) apply(cmd *cobra.Command, j *model.Job, now time.Time) error {
	set := func(name string, fn func() error) error {
		if cmd.Flags().Changed(name) {
			return fn()
		}
		return nil
	}
	steps := []struct {
		name string
		fn   func() error
	}{
		{"title", func() error { j.Title = f.title; return nil }},
		{"desc", func() error { j.Description = f.desc; return nil }},
		{"user", func() error { j.User = f.user; return nil }},
		{"path", func() error {
			p, err := filepath.Abs(f.path)
			j.Path = p
			return err
		}},
		{"sudo", func() error { j.Superuser = f.sudo; return nil }},
		{"sudo-mode", func() error { j.SudoMode = f.sudoMode; return nil }},
		{"command", func() error { j.Command, j.Script = f.command, ""; return nil }},
		{"script", func() error { j.Script, j.Command = f.script, ""; return nil }},
		{"shell", func() error { j.Interpreter = f.shell; return nil }},
		{"arg", func() error { j.Args = f.args; return nil }},
		{"env", func() error {
			j.Env = map[string]string{}
			for _, kv := range f.env {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid --env %q (want KEY=VALUE)", kv)
				}
				j.Env[k] = v
			}
			return nil
		}},
		{"timeout", func() error { d, err := parseDur(f.timeout); j.Timeout = d; return err }},
		{"remind", func() error { d, err := parseDur(f.remind); j.RemindBefore = d; return err }},
		{"catchup", func() error { j.Catchup = f.catchup; return nil }},
		{"tag", func() error { j.Tags = f.tags; return nil }},
		{"disarmed", func() error { j.Armed = !f.disarmed; return nil }},
		{"no-conditions", func() error {
			if f.noConditions {
				j.Conditions = nil
			}
			return nil
		}},
	}
	for _, s := range steps {
		if err := set(s.name, s.fn); err != nil {
			return fmt.Errorf("--%s: %w", s.name, err)
		}
	}
	if j.Superuser && j.SudoMode == "" {
		j.SudoMode = model.SudoExec
	}
	if scheduleChanged(cmd) {
		s, err := f.schedule(cmd, now)
		if err != nil {
			return err
		}
		j.Schedule = s
	}
	if changed(cmd, "when", "when-any") {
		c, err := buildConditions(f.when, f.whenAny)
		if err != nil {
			return err
		}
		j.Conditions = c
	}
	for name, list := range map[string][]string{"on-success": f.onSuccess, "on-fail": f.onFail, "on-error": f.onError, "on-unknown": f.onUnknown} {
		if !changed(cmd, name) {
			continue
		}
		event := strings.TrimPrefix(name, "on-")
		var actions []model.Action
		for _, s := range list {
			a, err := model.ParseAction(s)
			if err != nil {
				return fmt.Errorf("--%s: %w", name, err)
			}
			actions = append(actions, a)
		}
		switch event {
		case model.RunSuccess:
			j.Hooks.OnSuccess = actions
		case model.RunFail:
			j.Hooks.OnFail = actions
		case model.RunError:
			j.Hooks.OnError = actions
		case model.RunUnknown:
			j.Hooks.OnUnknown = actions
		}
	}
	return nil
}

func buildConditions(all, anyOf []string) (*model.Condition, error) {
	var leaves []model.Condition
	for _, s := range all {
		c, err := cond.Parse(s)
		if err != nil {
			return nil, err
		}
		leaves = append(leaves, c)
	}
	if len(anyOf) > 0 {
		var alts []model.Condition
		for _, s := range anyOf {
			c, err := cond.Parse(s)
			if err != nil {
				return nil, err
			}
			alts = append(alts, c)
		}
		if len(alts) == 1 {
			leaves = append(leaves, alts[0])
		} else {
			leaves = append(leaves, model.Condition{Any: alts})
		}
	}
	switch len(leaves) {
	case 0:
		return nil, nil
	case 1:
		return &leaves[0], nil
	}
	return &model.Condition{All: leaves}, nil
}

// loadSpec reads a job spec from a .toml or .json file.
func loadSpec(path string) (*model.Job, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j model.Job
	if strings.HasSuffix(path, ".json") {
		err = json.Unmarshal(b, &j)
	} else {
		_, err = toml.Decode(string(b), &j)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &j, nil
}

// exportSpec returns the definition part of a job (no runtime state).
func exportSpec(j *model.Job) *model.Job {
	c := j.Clone()
	c.Status, c.NextRun, c.LastRun, c.LastStatus, c.RemindedAt, c.PausedAt = "", nil, nil, "", nil, nil
	c.Paused = false
	if c.Schedule.In > 0 {
		c.Schedule.At = time.Time{} // re-resolved relative to the next registration
	}
	return c
}

func encodeSpec(j *model.Job, format string) (string, error) {
	if format == "json" {
		b, err := json.MarshalIndent(j, "", "  ")
		return string(b) + "\n", err
	}
	var sb strings.Builder
	err := toml.NewEncoder(&sb).Encode(j)
	return sb.String(), err
}
