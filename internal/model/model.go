// Package model defines the job specification shared by the store, the
// scheduler, the executor and the CLI. A job is stored as JSON and can be
// exported/imported as TOML.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Job lifecycle status.
const (
	StatusActive    = "active"
	StatusDone      = "done"
	StatusCancelled = "cancelled"
)

// Schedule kinds.
const (
	ScheduleOnce     = "once"
	ScheduleCron     = "cron"
	ScheduleInterval = "interval"
	ScheduleWatch    = "watch"
	ScheduleManual   = "manual"
)

// Superuser authorization modes.
const (
	SudoPreauth = "preauth" // job runs from the root (system) daemon
	SudoExec    = "exec"    // privileges are requested with pkexec at execution time
)

// Catch-up policies for runs missed while the daemon was down.
const (
	CatchupOnce = "once"
	CatchupSkip = "skip"
)

// Run outcomes. These are also the hook event names.
const (
	RunSuccess = "success"
	RunFail    = "fail"
	RunError   = "error"
	RunUnknown = "unknown"
	RunSkipped = "skipped"
	RunMissed  = "missed"
)

// Job is the full specification and state of a scheduled job.
type Job struct {
	UUID         string            `json:"uuid" toml:"uuid,omitempty"`
	Title        string            `json:"title" toml:"title"`
	Description  string            `json:"description,omitempty" toml:"description,omitempty"`
	RegisteredAt time.Time         `json:"registered_at" toml:"registered_at,omitempty"`
	Schedule     Schedule          `json:"schedule" toml:"schedule"`
	User         string            `json:"user,omitempty" toml:"user,omitempty"`
	Path         string            `json:"path" toml:"path"`
	Superuser    bool              `json:"superuser,omitempty" toml:"superuser,omitempty"`
	SudoMode     string            `json:"sudo_mode,omitempty" toml:"sudo_mode,omitempty"`
	Interpreter  string            `json:"interpreter,omitempty" toml:"interpreter,omitempty"`
	Command      string            `json:"command,omitempty" toml:"command,omitempty"`
	Script       string            `json:"script,omitempty" toml:"script,omitempty"`
	Args         []string          `json:"args,omitempty" toml:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty" toml:"env,omitempty"`
	Timeout      Duration          `json:"timeout,omitempty" toml:"timeout,omitempty,omitzero"`
	Conditions   *Condition        `json:"conditions,omitempty" toml:"conditions,omitempty"`
	Logic        []Step            `json:"logic,omitempty" toml:"logic,omitempty"`
	Hooks        Hooks             `json:"hooks,omitempty" toml:"hooks,omitempty"`
	RemindBefore Duration          `json:"remind_before,omitempty" toml:"remind_before,omitempty,omitzero"`
	Catchup      string            `json:"catchup,omitempty" toml:"catchup,omitempty"`
	Template     string            `json:"template,omitempty" toml:"template,omitempty"`
	Tags         []string          `json:"tags,omitempty" toml:"tags,omitempty"`

	// Runtime state.
	Status     string     `json:"status" toml:"status,omitempty"`
	Armed      bool       `json:"armed" toml:"armed"`
	Paused     bool       `json:"paused" toml:"paused"`
	PausedAt   *time.Time `json:"paused_at,omitempty" toml:"paused_at,omitempty"`
	NextRun    *time.Time `json:"next_run,omitempty" toml:"next_run,omitempty"`
	LastRun    *time.Time `json:"last_run,omitempty" toml:"last_run,omitempty"`
	LastStatus string     `json:"last_status,omitempty" toml:"last_status,omitempty"`
	RemindedAt *time.Time `json:"reminded_at,omitempty" toml:"reminded_at,omitempty"`
}

// Schedule describes when a job fires. Execution time is deliberately kept
// apart from Conditions.
type Schedule struct {
	Kind     string    `json:"kind" toml:"kind"`
	At       time.Time `json:"at,omitempty" toml:"at,omitempty"`
	In       Duration  `json:"in,omitempty" toml:"in,omitempty,omitzero"` // relative once-time, resolved at registration (templates)
	Cron    string    `json:"cron,omitempty" toml:"cron,omitempty"`
	Every    Duration  `json:"every,omitempty" toml:"every,omitempty,omitzero"`
	Watch    []string  `json:"watch,omitempty" toml:"watch,omitempty"`
	Events   []string  `json:"events,omitempty" toml:"events,omitempty"` // create, write, remove, rename, chmod
	Debounce Duration  `json:"debounce,omitempty" toml:"debounce,omitempty,omitzero"`
}

// Condition is a declarative condition tree. Exactly one of the combinators
// (All, Any, Not) or a primitive Type is expected per node.
//
// Primitive types:
//
//	file_exists   Path
//	dir_exists    Path
//	path_changed  Path            (modified since the job's last run)
//	command       Command         (exit status 0, bounded by Timeout)
//	env           Name, Value     (Value empty = variable is set)
//	time_window   From, To        ("HH:MM", may wrap midnight)
//	weekday       Days            ("mon".."sun")
//	job_status    Job, Status     (last run status of another job)
//	process       Name            (a process with that name is running)
type Condition struct {
	Type    string      `json:"type,omitempty" toml:"type,omitempty"`
	All     []Condition `json:"all,omitempty" toml:"all,omitempty"`
	Any     []Condition `json:"any,omitempty" toml:"any,omitempty"`
	Not     *Condition  `json:"not,omitempty" toml:"not,omitempty"`
	Path    string      `json:"path,omitempty" toml:"path,omitempty"`
	Command string      `json:"command,omitempty" toml:"command,omitempty"`
	Name    string      `json:"name,omitempty" toml:"name,omitempty"`
	Value   string      `json:"value,omitempty" toml:"value,omitempty"`
	From    string      `json:"from,omitempty" toml:"from,omitempty"`
	To      string      `json:"to,omitempty" toml:"to,omitempty"`
	Days    []string    `json:"days,omitempty" toml:"days,omitempty"`
	Job     string      `json:"job,omitempty" toml:"job,omitempty"`
	Status  string      `json:"status,omitempty" toml:"status,omitempty"`
	Timeout Duration    `json:"timeout,omitempty" toml:"timeout,omitempty,omitzero"`
}

// Step is one node of a job's control flow. A step does exactly one of:
// run a command, run the job's main command/script (Main), branch (If/Elif/
// Else) or loop (While/Do, bounded by MaxIter).
type Step struct {
	Name    string     `json:"name,omitempty" toml:"name,omitempty"`
	Run     string     `json:"run,omitempty" toml:"run,omitempty"`
	Main    bool       `json:"main,omitempty" toml:"main,omitempty"`
	If      *Condition `json:"if,omitempty" toml:"if,omitempty"`
	Then    []Step     `json:"then,omitempty" toml:"then,omitempty"`
	Elif    []Branch   `json:"elif,omitempty" toml:"elif,omitempty"`
	Else    []Step     `json:"else,omitempty" toml:"else,omitempty"`
	While   *Condition `json:"while,omitempty" toml:"while,omitempty"`
	Do      []Step     `json:"do,omitempty" toml:"do,omitempty"`
	MaxIter int        `json:"max_iter,omitempty" toml:"max_iter,omitempty"`
	// ContinueOnError keeps executing the following steps after a failure.
	ContinueOnError bool `json:"continue_on_error,omitempty" toml:"continue_on_error,omitempty"`
}

// Branch is an elif arm.
type Branch struct {
	If   *Condition `json:"if" toml:"if"`
	Then []Step     `json:"then" toml:"then"`
}

// Hooks maps run outcomes to extra actions.
type Hooks struct {
	OnSuccess []Action `json:"on_success,omitempty" toml:"on_success,omitempty"`
	OnFail    []Action `json:"on_fail,omitempty" toml:"on_fail,omitempty"`
	OnError   []Action `json:"on_error,omitempty" toml:"on_error,omitempty"`
	OnUnknown []Action `json:"on_unknown,omitempty" toml:"on_unknown,omitempty"`
}

// For returns the actions registered for a run outcome.
func (h Hooks) For(status string) []Action {
	switch status {
	case RunSuccess:
		return h.OnSuccess
	case RunFail:
		return h.OnFail
	case RunError:
		return h.OnError
	case RunUnknown:
		return h.OnUnknown
	}
	return nil
}

// Add appends an action for a run outcome.
func (h *Hooks) Add(status string, a Action) error {
	switch status {
	case RunSuccess:
		h.OnSuccess = append(h.OnSuccess, a)
	case RunFail:
		h.OnFail = append(h.OnFail, a)
	case RunError:
		h.OnError = append(h.OnError, a)
	case RunUnknown:
		h.OnUnknown = append(h.OnUnknown, a)
	default:
		return fmt.Errorf("unknown hook event %q", status)
	}
	return nil
}

// Count returns the total number of hook actions.
func (h Hooks) Count() int {
	return len(h.OnSuccess) + len(h.OnFail) + len(h.OnError) + len(h.OnUnknown)
}

// Action is a hook action: notify (desktop notification), webhook (HTTP POST
// of the run as JSON) or command (shell command).
type Action struct {
	Type    string `json:"type" toml:"type"`
	Message string `json:"message,omitempty" toml:"message,omitempty"`
	URL     string `json:"url,omitempty" toml:"url,omitempty"`
	Command string `json:"command,omitempty" toml:"command,omitempty"`
}

// ParseAction parses the CLI shorthand "notify:msg", "webhook:URL" or "cmd:command".
func ParseAction(s string) (Action, error) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok {
		return Action{}, fmt.Errorf("invalid hook %q (want notify:MSG, webhook:URL or cmd:COMMAND)", s)
	}
	switch kind {
	case "notify":
		return Action{Type: "notify", Message: rest}, nil
	case "webhook":
		return Action{Type: "webhook", URL: rest}, nil
	case "cmd", "command":
		return Action{Type: "command", Command: rest}, nil
	}
	return Action{}, fmt.Errorf("unknown hook type %q", kind)
}

// Run is one execution record in the history ledger.
type Run struct {
	ID         int64     `json:"id"`
	JobUUID    string    `json:"job_uuid"`
	Trigger    string    `json:"trigger"` // schedule, watch, manual, catchup
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Revision is an audited modification of a job.
type Revision struct {
	ID         int64     `json:"id"`
	JobUUID    string    `json:"job_uuid"`
	At         time.Time `json:"at"`
	Action     string    `json:"action"`
	Reason     string    `json:"reason"`
	ReasonDesc string    `json:"reason_desc,omitempty"`
	Actor      string    `json:"actor"`
	Before     string    `json:"before,omitempty"`
	After      string    `json:"after,omitempty"`
}

// Template is a reusable, partially filled job spec.
type Template struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Spec        Job       `json:"spec"`
	CreatedAt   time.Time `json:"created_at"`
}

// Validate checks the static consistency of a job spec.
func (j *Job) Validate() error {
	if strings.TrimSpace(j.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if j.Command == "" && j.Script == "" && len(j.Logic) == 0 {
		return fmt.Errorf("one of command, script or logic is required")
	}
	if j.Command != "" && j.Script != "" {
		return fmt.Errorf("command and script are mutually exclusive")
	}
	if j.Path == "" || !strings.HasPrefix(j.Path, "/") {
		return fmt.Errorf("path must be absolute, got %q", j.Path)
	}
	switch j.Schedule.Kind {
	case ScheduleOnce:
		if j.Schedule.At.IsZero() && j.Schedule.In <= 0 {
			return fmt.Errorf("once schedule needs an execution time")
		}
	case ScheduleCron:
		if j.Schedule.Cron == "" {
			return fmt.Errorf("cron schedule needs an expression")
		}
	case ScheduleInterval:
		if j.Schedule.Every.D() <= 0 {
			return fmt.Errorf("interval schedule needs a positive interval")
		}
	case ScheduleWatch:
		if len(j.Schedule.Watch) == 0 {
			return fmt.Errorf("watch schedule needs at least one path")
		}
	case ScheduleManual:
	default:
		return fmt.Errorf("unknown schedule kind %q", j.Schedule.Kind)
	}
	if j.Superuser && j.SudoMode != SudoPreauth && j.SudoMode != SudoExec {
		return fmt.Errorf("sudo_mode must be %q or %q", SudoPreauth, SudoExec)
	}
	if j.Catchup != "" && j.Catchup != CatchupOnce && j.Catchup != CatchupSkip {
		return fmt.Errorf("catchup must be %q or %q", CatchupOnce, CatchupSkip)
	}
	if j.Conditions != nil {
		if err := j.Conditions.Validate(); err != nil {
			return fmt.Errorf("conditions: %w", err)
		}
	}
	return validateSteps(j.Logic)
}

func validateSteps(steps []Step) error {
	for i, s := range steps {
		kinds := 0
		if s.Run != "" {
			kinds++
		}
		if s.Main {
			kinds++
		}
		if s.If != nil {
			kinds++
		}
		if s.While != nil {
			kinds++
		}
		if kinds != 1 {
			return fmt.Errorf("logic step %d must have exactly one of run, main, if, while", i)
		}
		for _, c := range []*Condition{s.If, s.While} {
			if c != nil {
				if err := c.Validate(); err != nil {
					return fmt.Errorf("logic step %d: %w", i, err)
				}
			}
		}
		for _, b := range s.Elif {
			if b.If == nil {
				return fmt.Errorf("logic step %d: elif without condition", i)
			}
			if err := validateSteps(b.Then); err != nil {
				return err
			}
		}
		for _, sub := range [][]Step{s.Then, s.Else, s.Do} {
			if err := validateSteps(sub); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate checks a condition tree.
func (c *Condition) Validate() error {
	nodes := 0
	if len(c.All) > 0 {
		nodes++
	}
	if len(c.Any) > 0 {
		nodes++
	}
	if c.Not != nil {
		nodes++
	}
	if c.Type != "" {
		nodes++
	}
	if nodes != 1 {
		return fmt.Errorf("condition must have exactly one of type, all, any, not")
	}
	for i := range c.All {
		if err := c.All[i].Validate(); err != nil {
			return err
		}
	}
	for i := range c.Any {
		if err := c.Any[i].Validate(); err != nil {
			return err
		}
	}
	if c.Not != nil {
		return c.Not.Validate()
	}
	if c.Type == "" {
		return nil
	}
	need := func(field, v string) error {
		if v == "" {
			return fmt.Errorf("%s condition needs %s", c.Type, field)
		}
		return nil
	}
	switch c.Type {
	case "file_exists", "dir_exists", "path_changed":
		return need("path", c.Path)
	case "command":
		return need("command", c.Command)
	case "env", "process":
		return need("name", c.Name)
	case "time_window":
		if err := need("from", c.From); err != nil {
			return err
		}
		return need("to", c.To)
	case "weekday":
		if len(c.Days) == 0 {
			return fmt.Errorf("weekday condition needs days")
		}
	case "job_status":
		if err := need("job", c.Job); err != nil {
			return err
		}
		return need("status", c.Status)
	default:
		return fmt.Errorf("unknown condition type %q", c.Type)
	}
	return nil
}

// Clone returns a deep copy through JSON.
func (j *Job) Clone() *Job {
	b, _ := json.Marshal(j)
	var c Job
	_ = json.Unmarshal(b, &c)
	return &c
}

// Duration is a time.Duration that (un)marshals as a string like "1h30m".
type Duration time.Duration

// D returns the value as time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string {
	if d == 0 {
		return ""
	}
	return time.Duration(d).String()
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*d = 0
		return nil
	}
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// ParseDuration extends time.ParseDuration with "d" (days) and "w" (weeks).
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var total time.Duration
	for _, unit := range []struct {
		suffix string
		mult   time.Duration
	}{{"w", 7 * 24 * time.Hour}, {"d", 24 * time.Hour}} {
		if i := strings.Index(s, unit.suffix); i > 0 {
			var n int
			if _, err := fmt.Sscanf(s[:i], "%d", &n); err != nil {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			total += time.Duration(n) * unit.mult
			s = s[i+1:]
		}
	}
	if s == "" {
		return total, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return total + d, nil
}
