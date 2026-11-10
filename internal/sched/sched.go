// Package sched computes fire times and applies pause/resume semantics.
package sched

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/sernik77/CR0N0S/internal/model"
)

var parser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ParseCron validates a cron expression (5 or 6 fields, or @daily-style descriptors).
func ParseCron(expr string) (cron.Schedule, error) {
	s, err := parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return s, nil
}

// Next returns the first fire time strictly after from, or nil when the job
// has no time-based schedule (watch, manual) or a one-shot job already ran.
func Next(j *model.Job, from time.Time) (*time.Time, error) {
	s := j.Schedule
	switch s.Kind {
	case model.ScheduleOnce:
		if j.LastRun != nil {
			return nil, nil
		}
		at := s.At
		return &at, nil
	case model.ScheduleCron:
		cs, err := ParseCron(s.Cron)
		if err != nil {
			return nil, err
		}
		n := cs.Next(from)
		return &n, nil
	case model.ScheduleInterval:
		every := s.Every.D()
		if every <= 0 {
			return nil, fmt.Errorf("interval must be positive")
		}
		base := j.RegisteredAt
		if j.LastRun != nil {
			base = *j.LastRun
		}
		n := base.Add(every)
		if !n.After(from) {
			k := from.Sub(base)/every + 1
			n = base.Add(k * every)
		}
		return &n, nil
	}
	return nil, nil
}

// Pause stops the job's timer.
func Pause(j *model.Job, now time.Time) {
	if j.Paused {
		return
	}
	j.Paused = true
	j.PausedAt = &now
}

// Resume restarts the job's timer. A one-shot job is shifted forward by the
// time spent paused; a recurring job resumes at its next regular fire time.
func Resume(j *model.Job, now time.Time) error {
	if !j.Paused {
		return nil
	}
	if j.Schedule.Kind == model.ScheduleOnce && j.PausedAt != nil {
		shift := now.Sub(*j.PausedAt)
		j.Schedule.At = j.Schedule.At.Add(shift).Truncate(time.Second)
	}
	j.Paused, j.PausedAt = false, nil
	n, err := Next(j, now)
	if err != nil {
		return err
	}
	j.NextRun = n
	return nil
}
