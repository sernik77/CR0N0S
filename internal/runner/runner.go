// Package runner executes jobs: it picks the interpreter, applies the job's
// user and privilege settings, walks the job's control flow and classifies
// the outcome.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sernik77/CR0N0S/internal/cond"
	"github.com/sernik77/CR0N0S/internal/model"
)

// MaxOutput caps the captured output stored per run.
const MaxOutput = 64 << 10

// DefaultMaxIter bounds while loops without an explicit max_iter.
const DefaultMaxIter = 100

// Result is the outcome of a job execution.
type Result struct {
	Status   string
	ExitCode int
	Output   string
	Err      string
}

// Runner executes jobs. Euid and CurrentUser describe the daemon's identity
// so privilege decisions can be tested without real escalation.
type Runner struct {
	Euid        int
	CurrentUser string
}

// New returns a runner for the current process identity.
func New() *Runner {
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return &Runner{Euid: os.Geteuid(), CurrentUser: name}
}

// interpreterArgs maps an interpreter to its "evaluate a string" flag.
var interpreterArgs = map[string][]string{
	"sh": {"-c"}, "bash": {"-c"}, "zsh": {"-c"}, "dash": {"-c"}, "ksh": {"-c"}, "fish": {"-c"},
	"pwsh": {"-NoProfile", "-Command"}, "powershell": {"-NoProfile", "-Command"},
	"python": {"-c"}, "python3": {"-c"}, "node": {"-e"}, "php": {"-r"}, "ruby": {"-e"}, "perl": {"-e"},
}

// Interpreters lists the supported interpreter names.
func Interpreters() []string {
	out := make([]string, 0, len(interpreterArgs))
	for k := range interpreterArgs {
		out = append(out, k)
	}
	return out
}

func interpreter(j *model.Job) string {
	if j.Interpreter != "" {
		return j.Interpreter
	}
	return "sh"
}

// CommandArgv builds the argv for an inline command.
func CommandArgv(j *model.Job, command string) ([]string, error) {
	in := interpreter(j)
	flags, ok := interpreterArgs[filepath.Base(in)]
	if !ok {
		return nil, fmt.Errorf("unsupported interpreter %q", in)
	}
	argv := append([]string{in}, flags...)
	argv = append(argv, command)
	if len(j.Args) > 0 && isShell(in) {
		argv = append(argv, "cr0") // $0
	}
	return append(argv, j.Args...), nil
}

func isShell(in string) bool {
	switch filepath.Base(in) {
	case "sh", "bash", "zsh", "dash", "ksh", "fish":
		return true
	}
	return false
}

// MainArgv builds the argv for the job's main command or script.
func MainArgv(j *model.Job) ([]string, error) {
	if j.Command != "" {
		return CommandArgv(j, j.Command)
	}
	if j.Script == "" {
		return nil, fmt.Errorf("job has no command or script")
	}
	script := j.Script
	if !filepath.IsAbs(script) {
		script = filepath.Join(j.Path, script)
	}
	argv := []string{script}
	if j.Interpreter != "" {
		argv = []string{j.Interpreter, script}
	}
	return append(argv, j.Args...), nil
}

// Prepared is a ready-to-start process description.
type Prepared struct {
	Argv []string
	Dir  string
	Env  []string
	Cred *syscall.Credential
}

// Prepare applies user, directory, environment and privilege settings.
func (r *Runner) Prepare(j *model.Job, argv []string) (*Prepared, error) {
	env := os.Environ()
	extra := []string{"CR0_JOB_UUID=" + j.UUID, "CR0_JOB_TITLE=" + j.Title}
	for k, v := range j.Env {
		extra = append(extra, k+"="+v)
	}
	p := &Prepared{Argv: argv, Dir: j.Path, Env: append(env, extra...)}

	if r.Euid == 0 {
		target := j.User
		if j.Superuser {
			target = "root"
		}
		if target == "" {
			return nil, errors.New("refusing to run a non-superuser job as root: set the job's user")
		}
		if target != "root" {
			cred, home, err := lookupCred(target)
			if err != nil {
				return nil, err
			}
			p.Cred = cred
			p.Env = append(p.Env, "HOME="+home, "USER="+target, "LOGNAME="+target)
		}
		return p, nil
	}

	if j.Superuser {
		if j.SudoMode != model.SudoExec {
			return nil, errors.New("pre-authorized superuser jobs must be run by the system (root) daemon")
		}
		envBin, err := exec.LookPath("env")
		if err != nil {
			return nil, err
		}
		// pkexec resets cwd and environment; restore them through env(1).
		wrapped := append([]string{"pkexec", envBin, "-C", j.Path}, extra...)
		p.Argv = append(wrapped, argv...)
		return p, nil
	}
	if j.User != "" && j.User != r.CurrentUser {
		return nil, fmt.Errorf("running as user %q requires the system (root) daemon", j.User)
	}
	return p, nil
}

func lookupCred(name string) (*syscall.Credential, string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, "", err
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	var groups []uint32
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				groups = append(groups, uint32(n))
			}
		}
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}, u.HomeDir, nil
}

type capped struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := MaxOutput - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

// Exec starts a prepared process and classifies its outcome.
func Exec(ctx context.Context, p *Prepared, timeout time.Duration) Result {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var out capped
	cmd := exec.CommandContext(ctx, p.Argv[0], p.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = p.Dir, p.Env, &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: p.Cred}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	err := cmd.Run()
	res := Result{Output: out.buf.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
		res.Status = model.RunSuccess
	case ctx.Err() == context.DeadlineExceeded:
		res.Status, res.ExitCode, res.Err = model.RunError, -1, fmt.Sprintf("timed out after %s", timeout)
	case errors.As(err, &ee):
		ws, _ := ee.Sys().(syscall.WaitStatus)
		if ws.Signaled() {
			res.Status, res.ExitCode, res.Err = model.RunUnknown, -1, "killed by "+ws.Signal().String()
		} else {
			res.Status, res.ExitCode = model.RunFail, ee.ExitCode()
			// pkexec: 126 = authorization dismissed/denied, 127 = not authorized.
			if p.Argv[0] == "pkexec" && (res.ExitCode == 126 || res.ExitCode == 127) {
				res.Status, res.Err = model.RunError, "pkexec authorization failed; the job did not run"
			}
		}
	default:
		res.Status, res.ExitCode, res.Err = model.RunError, -1, err.Error()
	}
	return res
}

// Run executes a job: its logic steps if any, otherwise its main command.
func (r *Runner) Run(ctx context.Context, j *model.Job, env cond.Env) Result {
	if len(j.Logic) == 0 {
		return r.runArgv(ctx, j, nil)
	}
	var log bytes.Buffer
	res := r.steps(ctx, j, j.Logic, env, &log)
	if res.Status == "" {
		res.Status = model.RunSuccess
	}
	res.Output = truncate(log.String())
	return res
}

func truncate(s string) string {
	if len(s) > MaxOutput {
		return s[:MaxOutput]
	}
	return s
}

func (r *Runner) runArgv(ctx context.Context, j *model.Job, argv []string) Result {
	var err error
	if argv == nil {
		argv, err = MainArgv(j)
	}
	if err != nil {
		return Result{Status: model.RunError, ExitCode: -1, Err: err.Error()}
	}
	p, err := r.Prepare(j, argv)
	if err != nil {
		return Result{Status: model.RunError, ExitCode: -1, Err: err.Error()}
	}
	return Exec(ctx, p, j.Timeout.D())
}

// steps walks the control flow. An empty Status means "no step failed".
func (r *Runner) steps(ctx context.Context, j *model.Job, steps []model.Step, env cond.Env, log *bytes.Buffer) Result {
	var last Result
	for _, s := range steps {
		var res Result
		switch {
		case s.Run != "" || s.Main:
			label, argv := s.Run, []string(nil)
			if s.Main {
				label = "<main>"
			} else {
				var err error
				if argv, err = CommandArgv(j, s.Run); err != nil {
					res = Result{Status: model.RunError, ExitCode: -1, Err: err.Error()}
					break
				}
			}
			fmt.Fprintf(log, "--- step %s: %s\n", stepName(s), label)
			res = r.runArgv(ctx, j, argv)
			log.WriteString(res.Output)
			if res.Status == model.RunSuccess {
				res.Status = ""
			}
		case s.If != nil:
			branch := s.Else
			if cond.Evaluate(s.If, env).Met {
				branch = s.Then
			} else {
				for _, b := range s.Elif {
					if cond.Evaluate(b.If, env).Met {
						branch = b.Then
						break
					}
				}
			}
			res = r.steps(ctx, j, branch, env, log)
		case s.While != nil:
			max := s.MaxIter
			if max <= 0 {
				max = DefaultMaxIter
			}
			i := 0
			for ; i < max && cond.Evaluate(s.While, env).Met; i++ {
				if res = r.steps(ctx, j, s.Do, env, log); res.Status != "" {
					break
				}
			}
			if i == max && res.Status == "" {
				fmt.Fprintf(log, "--- while loop stopped after max_iter=%d\n", max)
			}
		}
		if res.Status != "" {
			if res.Err != "" {
				fmt.Fprintf(log, "--- error: %s\n", res.Err)
			}
			if !s.ContinueOnError {
				return res
			}
			last = res
		}
		if ctx.Err() != nil {
			return Result{Status: model.RunError, ExitCode: -1, Err: ctx.Err().Error()}
		}
	}
	return last
}

func stepName(s model.Step) string {
	if s.Name != "" {
		return s.Name
	}
	return "-"
}
