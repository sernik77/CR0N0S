package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sernik77/CR0N0S/internal/model"
)

// Notify shows a desktop notification through notify-send. When the daemon
// runs as root there is usually no session bus, so this is best effort.
func Notify(title, body, urgency string) error {
	if urgency == "" {
		urgency = "normal"
	}
	return exec.Command("notify-send", "-a", "cr0", "-u", urgency, title, body).Run()
}

func expand(msg string, j *model.Job, run *model.Run) string {
	return strings.NewReplacer(
		"{title}", j.Title, "{uuid}", j.UUID, "{status}", run.Status,
		"{exit}", strconv.Itoa(run.ExitCode), "{error}", run.Error,
	).Replace(msg)
}

// FireHooks runs the hook actions registered for the run's status and
// returns one error per failed action.
func FireHooks(j *model.Job, run *model.Run) []error {
	var errs []error
	for _, a := range j.Hooks.For(run.Status) {
		if err := fire(a, j, run); err != nil {
			errs = append(errs, fmt.Errorf("%s hook: %w", a.Type, err))
		}
	}
	return errs
}

func fire(a model.Action, j *model.Job, run *model.Run) error {
	switch a.Type {
	case "notify":
		msg := a.Message
		if msg == "" {
			msg = "{title}: {status} (exit {exit})"
		}
		urgency := "normal"
		if run.Status != model.RunSuccess {
			urgency = "critical"
		}
		return Notify("cr0: "+j.Title, expand(msg, j, run), urgency)
	case "webhook":
		body, _ := json.Marshal(map[string]any{
			"job": map[string]string{"uuid": j.UUID, "title": j.Title, "path": j.Path}, "run": run,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "cr0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("webhook returned %s", resp.Status)
		}
		return nil
	case "command":
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", a.Command)
		cmd.Dir = j.Path
		cmd.Env = append(os.Environ(),
			"CR0_JOB_UUID="+j.UUID, "CR0_JOB_TITLE="+j.Title, "CR0_STATUS="+run.Status,
			"CR0_EXIT_CODE="+strconv.Itoa(run.ExitCode), "CR0_ERROR="+run.Error)
		return cmd.Run()
	}
	return fmt.Errorf("unknown hook type %q", a.Type)
}
