// Package core holds the operations shared by the CLI, the TUI and the
// daemon: executing a job into the ledger and inspecting its conditions.
package core

import (
	"context"
	"fmt"
	"time"

	"github.com/sernik77/CR0N0S/internal/cond"
	"github.com/sernik77/CR0N0S/internal/model"
	"github.com/sernik77/CR0N0S/internal/runner"
	"github.com/sernik77/CR0N0S/internal/sched"
	"github.com/sernik77/CR0N0S/internal/store"
)

// Trigger names.
const (
	TriggerSchedule = "schedule"
	TriggerWatch    = "watch"
	TriggerManual   = "manual"
	TriggerCatchup  = "catchup"
)

// EnvFor builds the condition environment for a job evaluated at "at".
func EnvFor(st *store.Store, j *model.Job, at time.Time) cond.Env {
	return cond.Env{
		Now:     at,
		LastRun: j.LastRun,
		Dir:     j.Path,
		JobStatus: func(id string) (string, error) {
			other, err := st.Get(id)
			if err != nil {
				return "", err
			}
			return other.LastStatus, nil
		},
	}
}

// Inspection is the inspector's view of a job's conditions.
type Inspection struct {
	cond.Summary
	// EvaluatedAt is the moment time-based conditions were evaluated for:
	// the next execution time if known, otherwise now.
	EvaluatedAt time.Time `json:"evaluated_at"`
	// Dependencies reports, for each job this one waits on, whether that
	// job's own conditions are currently met.
	Dependencies map[string]bool `json:"dependencies,omitempty"`
}

// Inspect evaluates a job's conditions as they would stand at its next
// execution if nothing else changes. Execution time is not a condition.
func Inspect(st *store.Store, j *model.Job, now time.Time) Inspection {
	at := now
	if j.NextRun != nil && j.NextRun.After(now) {
		at = *j.NextRun
	}
	ins := Inspection{Summary: cond.Summarize(j.Conditions, EnvFor(st, j, at)), EvaluatedAt: at}
	for _, id := range ins.DependsOn {
		other, err := st.Get(id)
		if err != nil || other.UUID == j.UUID {
			continue
		}
		if ins.Dependencies == nil {
			ins.Dependencies = map[string]bool{}
		}
		ins.Dependencies[id] = cond.Summarize(other.Conditions, EnvFor(st, other, now)).Met
	}
	return ins
}

// Execute runs a job (honouring armed state and conditions), records the
// run in the ledger, fires hooks and updates the job's last-run state.
// The caller is responsible for having advanced NextRun beforehand.
func Execute(ctx context.Context, st *store.Store, r *runner.Runner, j *model.Job, trigger string) (*model.Run, error) {
	start := time.Now()
	run := &model.Run{JobUUID: j.UUID, Trigger: trigger, StartedAt: start}
	env := EnvFor(st, j, start)

	switch sum := cond.Summarize(j.Conditions, env); {
	case !j.Armed && trigger != TriggerManual:
		run.Status, run.Error = model.RunSkipped, "job is disarmed"
	case !sum.Met:
		run.Status, run.Error = model.RunSkipped, fmt.Sprintf("conditions not met (%d/%d)", sum.MetCount, sum.Count)
	default:
		res := r.Run(ctx, j, env)
		run.Status, run.ExitCode, run.Output, run.Error = res.Status, res.ExitCode, res.Output, res.Err
	}
	run.FinishedAt = time.Now()
	if err := st.AddRun(run); err != nil {
		return run, err
	}
	if run.Status != model.RunSkipped {
		for _, err := range runner.FireHooks(j, run) {
			run.Error += "; " + err.Error()
		}
	}
	_, err := st.UpdateState(j.UUID, func(cur *model.Job) error {
		if run.Status != model.RunSkipped {
			cur.LastStatus = run.Status
		}
		if trigger == TriggerManual {
			if cur.Schedule.Kind != model.ScheduleOnce {
				cur.LastRun = &start
			}
			return nil
		}
		cur.LastRun = &start
		if cur.Schedule.Kind == model.ScheduleOnce {
			cur.Status, cur.NextRun = model.StatusDone, nil
		}
		return nil
	})
	return run, err
}

// Claim advances a due job's NextRun before it executes so it cannot fire
// twice. It returns false when the job is no longer due.
func Claim(st *store.Store, id string, now time.Time) (*model.Job, bool, error) {
	claimed := false
	j, err := st.UpdateState(id, func(cur *model.Job) error {
		if cur.Status != model.StatusActive || cur.Paused || cur.NextRun == nil || cur.NextRun.After(now) {
			return nil
		}
		claimed = true
		if cur.Schedule.Kind == model.ScheduleOnce {
			cur.NextRun = nil
			return nil
		}
		n, err := sched.Next(cur, now)
		cur.NextRun = n
		return err
	})
	return j, claimed, err
}
