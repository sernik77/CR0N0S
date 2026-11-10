package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sernik77/CR0N0S/internal/cond"
	"github.com/sernik77/CR0N0S/internal/model"
)

func job(dir string) *model.Job {
	return &model.Job{UUID: "u", Title: "t", Path: dir}
}

func TestPrepareUnprivileged(t *testing.T) {
	r := &Runner{Euid: 1000, CurrentUser: "me"}
	j := job("/srv")
	argv := []string{"sh", "-c", "id"}

	j.User = "other"
	if _, err := r.Prepare(j, argv); err == nil {
		t.Error("running as another user without root must fail")
	}

	j.User, j.Superuser, j.SudoMode = "", true, model.SudoPreauth
	if _, err := r.Prepare(j, argv); err == nil {
		t.Error("preauth superuser job outside the system daemon must fail")
	}

	j.SudoMode = model.SudoExec
	p, err := r.Prepare(j, argv)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(p.Argv, " ")
	if !strings.HasPrefix(got, "pkexec ") || !strings.Contains(got, "-C /srv CR0_JOB_UUID=u") || !strings.HasSuffix(got, "sh -c id") {
		t.Errorf("pkexec wrapping: %s", got)
	}
}

func TestPrepareAsRoot(t *testing.T) {
	r := &Runner{Euid: 0, CurrentUser: "root"}
	j := job("/srv")
	j.User = "root"
	p, err := r.Prepare(j, []string{"true"})
	if err != nil || p.Cred != nil || p.Argv[0] != "true" {
		t.Fatalf("root job: %+v %v", p, err)
	}
	j.User = ""
	if _, err := r.Prepare(j, []string{"true"}); err == nil {
		t.Error("non-superuser job without a user must not run as root")
	}
	j.Superuser = true
	if _, err := r.Prepare(j, []string{"true"}); err != nil {
		t.Errorf("superuser job as root: %v", err)
	}
	j.Superuser, j.User = false, "cr0-no-such-user"
	if _, err := r.Prepare(j, []string{"true"}); err == nil {
		t.Error("unknown user accepted")
	}
}

func TestInterpreters(t *testing.T) {
	j := job("/")
	j.Interpreter, j.Args = "bash", []string{"a"}
	argv, _ := CommandArgv(j, "echo $1")
	if strings.Join(argv, "|") != "bash|-c|echo $1|cr0|a" {
		t.Errorf("bash argv %v", argv)
	}
	j.Interpreter, j.Args = "node", nil
	if argv, _ = CommandArgv(j, "1"); argv[1] != "-e" {
		t.Errorf("node argv %v", argv)
	}
	j.Interpreter = "cobol"
	if _, err := CommandArgv(j, "x"); err == nil {
		t.Error("unknown interpreter accepted")
	}
	j.Interpreter, j.Script = "python3", "s.py"
	j.Command = ""
	if argv, _ = MainArgv(j); argv[1] != "/s.py" {
		t.Errorf("script argv %v", argv)
	}
}

func run(t *testing.T, j *model.Job) Result {
	t.Helper()
	return New().Run(context.Background(), j, cond.Env{Now: time.Now(), Dir: j.Path})
}

func TestOutcomes(t *testing.T) {
	dir := t.TempDir()
	j := job(dir)
	for cmd, want := range map[string]string{"echo ok": model.RunSuccess, "exit 4": model.RunFail, "kill -9 $$": model.RunUnknown} {
		j.Command = cmd
		if res := run(t, j); res.Status != want {
			t.Errorf("%q: %s (%+v)", cmd, res.Status, res)
		}
	}
	j.Command, j.Timeout = "sleep 5", model.Duration(200*time.Millisecond)
	if res := run(t, j); res.Status != model.RunError || !strings.Contains(res.Err, "timed out") {
		t.Errorf("timeout: %+v", res)
	}
}

func TestLogic(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	j := job(dir)
	j.Command, j.Schedule.Kind = "echo main", model.ScheduleManual
	j.Logic = []model.Step{
		{If: &model.Condition{Type: "file_exists", Path: "missing"},
			Then: []model.Step{{Run: "echo then"}},
			Elif: []model.Branch{{If: &model.Condition{Type: "dir_exists", Path: "."}, Then: []model.Step{{Run: "echo elif"}}}},
			Else: []model.Step{{Run: "echo else"}}},
		{While: &model.Condition{Not: &model.Condition{Type: "command", Command: "test -f n && test $(wc -l < n) -ge 3"}},
			Do: []model.Step{{Run: "echo x >> " + counter}}},
		{Main: true},
		{Run: "exit 2", ContinueOnError: true},
		{Run: "echo after"},
	}
	if err := j.Validate(); err != nil {
		t.Fatal(err)
	}
	res := run(t, j)
	b, _ := os.ReadFile(counter)
	if strings.Count(string(b), "x") != 3 {
		t.Errorf("while ran %d times", strings.Count(string(b), "x"))
	}
	for _, want := range []string{"elif", "main", "after"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output missing %q:\n%s", want, res.Output)
		}
	}
	if strings.Contains(res.Output, "then\n") || strings.Contains(res.Output, "else\n") {
		t.Errorf("wrong branch taken:\n%s", res.Output)
	}
	if res.Status != model.RunFail || res.ExitCode != 2 {
		t.Errorf("continue_on_error failure must still be reported: %+v", res)
	}
}
