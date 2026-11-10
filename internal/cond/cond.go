// Package cond evaluates condition trees. Evaluation never changes job state;
// the only side effect is running the probe of a "command" condition.
package cond

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sernik77/CR0N0S/internal/model"
)

// Env is the evaluation context.
type Env struct {
	// Now is the moment time-based conditions are evaluated for. The
	// inspector sets it to the job's next execution time, so "met" reflects
	// the expected state at execution if nothing else changes.
	Now time.Time
	// LastRun is the evaluated job's last run (for path_changed).
	LastRun *time.Time
	// Dir is the working directory for relative paths and probe commands.
	Dir string
	// JobStatus returns the last run status of another job.
	JobStatus func(id string) (string, error)
}

// Result is the outcome of one condition node.
type Result struct {
	Desc     string   `json:"desc"`
	Met      bool     `json:"met"`
	Err      string   `json:"error,omitempty"`
	Children []Result `json:"children,omitempty"`
}

// Summary aggregates a condition tree for listings and the inspector.
type Summary struct {
	Count     int      `json:"count"`
	MetCount  int      `json:"met_count"`
	Met       bool     `json:"met"`
	DependsOn []string `json:"depends_on,omitempty"` // other jobs this job waits on
	Tree      *Result  `json:"tree,omitempty"`
}

// Summarize evaluates c. A nil tree has zero conditions and is met.
func Summarize(c *model.Condition, env Env) Summary {
	if c == nil {
		return Summary{Met: true}
	}
	r := Evaluate(c, env)
	s := Summary{Met: r.Met, Tree: &r}
	walk(c, &r, &s, false)
	return s
}

// walk counts primitive conditions; a negated primitive is satisfied when
// the primitive itself is not met.
func walk(c *model.Condition, r *Result, s *Summary, negated bool) {
	if c.Type != "" {
		s.Count++
		if r.Met != negated {
			s.MetCount++
		}
		if c.Type == "job_status" {
			s.DependsOn = append(s.DependsOn, c.Job)
		}
		return
	}
	if c.Not != nil {
		if len(r.Children) == 1 {
			walk(c.Not, &r.Children[0], s, !negated)
		}
		return
	}
	children := append(append([]model.Condition{}, c.All...), c.Any...)
	for i := range children {
		if i < len(r.Children) {
			walk(&children[i], &r.Children[i], s, negated)
		}
	}
}

// Evaluate evaluates a condition node.
func Evaluate(c *model.Condition, env Env) Result {
	switch {
	case len(c.All) > 0:
		r := Result{Desc: "all of", Met: true}
		for i := range c.All {
			cr := Evaluate(&c.All[i], env)
			r.Met = r.Met && cr.Met
			r.Children = append(r.Children, cr)
		}
		return r
	case len(c.Any) > 0:
		r := Result{Desc: "any of"}
		for i := range c.Any {
			cr := Evaluate(&c.Any[i], env)
			r.Met = r.Met || cr.Met
			r.Children = append(r.Children, cr)
		}
		return r
	case c.Not != nil:
		cr := Evaluate(c.Not, env)
		return Result{Desc: "not", Met: !cr.Met, Children: []Result{cr}}
	}
	met, err := primitive(c, env)
	r := Result{Desc: Describe(c), Met: met && err == nil}
	if err != nil {
		r.Err = err.Error()
	}
	return r
}

func (e Env) abs(p string) string {
	p = os.ExpandEnv(p)
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[2:])
		}
	}
	if !filepath.IsAbs(p) && e.Dir != "" {
		p = filepath.Join(e.Dir, p)
	}
	return p
}

func primitive(c *model.Condition, env Env) (bool, error) {
	switch c.Type {
	case "file_exists", "dir_exists":
		fi, err := os.Stat(env.abs(c.Path))
		if err != nil {
			return false, nil
		}
		return fi.IsDir() == (c.Type == "dir_exists"), nil
	case "path_changed":
		fi, err := os.Stat(env.abs(c.Path))
		if err != nil {
			return false, nil
		}
		return env.LastRun == nil || fi.ModTime().After(*env.LastRun), nil
	case "command":
		timeout := c.Timeout.D()
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", c.Command)
		cmd.Dir = env.Dir
		return cmd.Run() == nil, nil
	case "env":
		v, ok := os.LookupEnv(c.Name)
		if c.Value == "" {
			return ok, nil
		}
		return ok && v == c.Value, nil
	case "time_window":
		return inWindow(env.Now, c.From, c.To)
	case "weekday":
		day := strings.ToLower(env.Now.Weekday().String()[:3])
		for _, d := range c.Days {
			if strings.HasPrefix(strings.ToLower(d), day) {
				return true, nil
			}
		}
		return false, nil
	case "job_status":
		if env.JobStatus == nil {
			return false, fmt.Errorf("job lookup unavailable")
		}
		st, err := env.JobStatus(c.Job)
		if err != nil {
			return false, err
		}
		return st == c.Status, nil
	case "process":
		return processRunning(c.Name), nil
	}
	return false, fmt.Errorf("unknown condition type %q", c.Type)
}

func parseHM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("invalid time %q (want HH:MM)", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func inWindow(now time.Time, from, to string) (bool, error) {
	f, err := parseHM(from)
	if err != nil {
		return false, err
	}
	t, err := parseHM(to)
	if err != nil {
		return false, err
	}
	m := now.Hour()*60 + now.Minute()
	if f <= t {
		return m >= f && m < t, nil
	}
	return m >= f || m < t, nil // wraps midnight
}

func processRunning(name string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err == nil && strings.TrimSpace(string(b)) == name {
			return true
		}
	}
	return false
}

// Describe renders a primitive condition as text.
func Describe(c *model.Condition) string {
	switch c.Type {
	case "file_exists", "dir_exists", "path_changed":
		return fmt.Sprintf("%s %s", c.Type, c.Path)
	case "command":
		return fmt.Sprintf("command `%s` succeeds", c.Command)
	case "env":
		if c.Value == "" {
			return fmt.Sprintf("env %s is set", c.Name)
		}
		return fmt.Sprintf("env %s == %q", c.Name, c.Value)
	case "time_window":
		return fmt.Sprintf("time between %s and %s", c.From, c.To)
	case "weekday":
		return "weekday in " + strings.Join(c.Days, ",")
	case "job_status":
		return fmt.Sprintf("job %s last status == %s", c.Job, c.Status)
	case "process":
		return fmt.Sprintf("process %s is running", c.Name)
	}
	return c.Type
}

// Parse parses the CLI shorthand for a primitive condition:
//
//	file:PATH  dir:PATH  changed:PATH  cmd:COMMAND  env:NAME[=VALUE]
//	time:HH:MM-HH:MM  days:mon,tue  job:UUID[=STATUS]  proc:NAME
//
// A leading "!" negates the condition.
func Parse(s string) (model.Condition, error) {
	if strings.HasPrefix(s, "!") {
		inner, err := Parse(s[1:])
		if err != nil {
			return inner, err
		}
		return model.Condition{Not: &inner}, nil
	}
	kind, arg, ok := strings.Cut(s, ":")
	if !ok || arg == "" {
		return model.Condition{}, fmt.Errorf("invalid condition %q", s)
	}
	var c model.Condition
	switch kind {
	case "file":
		c = model.Condition{Type: "file_exists", Path: arg}
	case "dir":
		c = model.Condition{Type: "dir_exists", Path: arg}
	case "changed":
		c = model.Condition{Type: "path_changed", Path: arg}
	case "cmd":
		c = model.Condition{Type: "command", Command: arg}
	case "env":
		n, v, _ := strings.Cut(arg, "=")
		c = model.Condition{Type: "env", Name: n, Value: v}
	case "time":
		f, t, ok := strings.Cut(arg, "-")
		if !ok {
			return c, fmt.Errorf("invalid time window %q (want HH:MM-HH:MM)", arg)
		}
		c = model.Condition{Type: "time_window", From: f, To: t}
	case "days":
		c = model.Condition{Type: "weekday", Days: strings.Split(arg, ",")}
	case "job":
		id, st, ok := strings.Cut(arg, "=")
		if !ok {
			st = model.RunSuccess
		}
		c = model.Condition{Type: "job_status", Job: id, Status: st}
	case "proc":
		c = model.Condition{Type: "process", Name: arg}
	default:
		return c, fmt.Errorf("unknown condition kind %q", kind)
	}
	return c, c.Validate()
}
