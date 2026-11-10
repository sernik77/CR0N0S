// Package daemon is the scheduler loop: it polls the database, fires due
// jobs, sends reminders, applies the catch-up policy and watches paths.
package daemon

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/sernik77/CR0N0S/internal/core"
	"github.com/sernik77/CR0N0S/internal/model"
	"github.com/sernik77/CR0N0S/internal/runner"
	"github.com/sernik77/CR0N0S/internal/sched"
	"github.com/sernik77/CR0N0S/internal/store"
)

// MissedGrace is how late a run may be before it counts as missed.
const MissedGrace = time.Minute

// DefaultDebounce is the quiet period after a watched path changes.
const DefaultDebounce = 2 * time.Second

// Daemon schedules and executes jobs.
type Daemon struct {
	Store  *store.Store
	Runner *runner.Runner
	Log    *log.Logger
	Poll   time.Duration

	wg      sync.WaitGroup
	mu      sync.Mutex
	running map[string]bool
	pending map[string]time.Time // watch job -> last event
	watcher *fsnotify.Watcher
	watched map[string][]string // path -> job uuids
}

// New creates a daemon.
func New(st *store.Store, r *runner.Runner, l *log.Logger) *Daemon {
	return &Daemon{Store: st, Runner: r, Log: l, Poll: time.Second,
		running: map[string]bool{}, pending: map[string]time.Time{}, watched: map[string][]string{}}
}

// Run loops until ctx is cancelled, then waits for running jobs.
func (d *Daemon) Run(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		d.Log.Printf("file watching disabled: %v", err)
	} else {
		d.watcher = w
		defer w.Close()
		go d.watchLoop(ctx)
	}
	d.Log.Printf("cr0 daemon started (db %s)", d.Store.Path)
	t := time.NewTicker(d.Poll)
	defer t.Stop()
	for {
		d.Tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			d.Log.Printf("stopping, waiting for running jobs")
			d.wg.Wait()
			return nil
		case <-t.C:
		}
	}
}

// Wait blocks until all started jobs have finished.
func (d *Daemon) Wait() { d.wg.Wait() }

// Tick performs one scheduling pass.
func (d *Daemon) Tick(ctx context.Context, now time.Time) {
	jobs, err := d.Store.Active()
	if err != nil {
		d.Log.Printf("load jobs: %v", err)
		return
	}
	d.reconcileWatches(jobs)
	for _, j := range jobs {
		if j.Paused {
			continue
		}
		if j.NextRun == nil {
			d.initNext(j, now)
			continue
		}
		d.remind(j, now)
		if j.NextRun.After(now) || d.isRunning(j.UUID) {
			continue
		}
		late := now.Sub(*j.NextRun) > MissedGrace
		claimed, ok, err := core.Claim(d.Store, j.UUID, now)
		if err != nil || !ok {
			if err != nil {
				d.Log.Printf("claim %s: %v", j.UUID, err)
			}
			continue
		}
		if late && claimed.Catchup == model.CatchupSkip {
			d.recordMissed(claimed, now)
			continue
		}
		trigger := core.TriggerSchedule
		if late {
			trigger = core.TriggerCatchup
		}
		d.start(ctx, claimed, trigger)
	}
	d.firePending(ctx, now)
}

// initNext computes NextRun for time-based jobs that have none yet.
func (d *Daemon) initNext(j *model.Job, now time.Time) {
	if j.Schedule.Kind != model.ScheduleCron && j.Schedule.Kind != model.ScheduleInterval {
		return
	}
	if _, err := d.Store.UpdateState(j.UUID, func(cur *model.Job) error {
		n, err := sched.Next(cur, now)
		cur.NextRun = n
		return err
	}); err != nil {
		d.Log.Printf("schedule %s: %v", j.UUID, err)
	}
}

func (d *Daemon) remind(j *model.Job, now time.Time) {
	before := j.RemindBefore.D()
	if before <= 0 || !j.Armed || j.NextRun.Before(now) {
		return
	}
	window := j.NextRun.Add(-before)
	if now.Before(window) || (j.RemindedAt != nil && !j.RemindedAt.Before(window)) {
		return
	}
	msg := j.Title + " runs at " + j.NextRun.Format("15:04:05") + " in " + j.Path
	if err := runner.Notify("cr0 reminder", msg, "normal"); err != nil {
		d.Log.Printf("reminder %s: %v", j.UUID, err)
	}
	d.Store.UpdateState(j.UUID, func(cur *model.Job) error { cur.RemindedAt = &now; return nil })
}

func (d *Daemon) recordMissed(j *model.Job, now time.Time) {
	d.Store.AddRun(&model.Run{JobUUID: j.UUID, Trigger: core.TriggerCatchup, StartedAt: now, FinishedAt: now,
		Status: model.RunMissed, Error: "daemon was not running at fire time (catchup=skip)"})
	if j.Schedule.Kind == model.ScheduleOnce {
		d.Store.UpdateState(j.UUID, func(cur *model.Job) error { cur.Status = model.StatusDone; cur.LastRun = &now; return nil })
	}
}

func (d *Daemon) isRunning(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running[id]
}

func (d *Daemon) start(ctx context.Context, j *model.Job, trigger string) {
	d.mu.Lock()
	if d.running[j.UUID] {
		d.mu.Unlock()
		return
	}
	d.running[j.UUID] = true
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() { d.mu.Lock(); delete(d.running, j.UUID); d.mu.Unlock() }()
		d.Log.Printf("run %s %q (%s)", j.UUID[:8], j.Title, trigger)
		run, err := core.Execute(ctx, d.Store, d.Runner, j, trigger)
		if err != nil {
			d.Log.Printf("record %s: %v", j.UUID, err)
		}
		d.Log.Printf("done %s %q: %s exit=%d %s", j.UUID[:8], j.Title, run.Status, run.ExitCode, run.Error)
	}()
}

func (d *Daemon) reconcileWatches(jobs []*model.Job) {
	if d.watcher == nil {
		return
	}
	want := map[string][]string{}
	for _, j := range jobs {
		if j.Schedule.Kind != model.ScheduleWatch || j.Paused {
			continue
		}
		for _, p := range j.Schedule.Watch {
			if !filepath.IsAbs(p) {
				p = filepath.Join(j.Path, p)
			}
			want[p] = append(want[p], j.UUID)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for p := range d.watched {
		if _, ok := want[p]; !ok {
			d.watcher.Remove(p)
		}
	}
	for p := range want {
		if _, ok := d.watched[p]; !ok {
			if err := d.watcher.Add(p); err != nil {
				d.Log.Printf("watch %s: %v", p, err)
				delete(want, p) // retried on the next tick
			}
		}
	}
	d.watched = want
}

func (d *Daemon) watchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-d.watcher.Errors:
			if !ok {
				return
			}
			d.Log.Printf("watcher: %v", err)
		case ev, ok := <-d.watcher.Events:
			if !ok {
				return
			}
			d.onEvent(ev)
		}
	}
}

func (d *Daemon) onEvent(ev fsnotify.Event) {
	d.mu.Lock()
	ids := append(d.watched[ev.Name], d.watched[filepath.Dir(ev.Name)]...)
	d.mu.Unlock()
	for _, id := range ids {
		j, err := d.Store.Get(id)
		if err != nil || !eventMatches(j.Schedule.Events, ev.Op) {
			continue
		}
		d.mu.Lock()
		d.pending[id] = time.Now()
		d.mu.Unlock()
	}
}

func eventMatches(filter []string, op fsnotify.Op) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		switch strings.ToLower(f) {
		case "create":
			if op.Has(fsnotify.Create) {
				return true
			}
		case "write", "modify":
			if op.Has(fsnotify.Write) {
				return true
			}
		case "remove", "delete":
			if op.Has(fsnotify.Remove) {
				return true
			}
		case "rename", "move":
			if op.Has(fsnotify.Rename) {
				return true
			}
		case "chmod", "attrib":
			if op.Has(fsnotify.Chmod) {
				return true
			}
		}
	}
	return false
}

func (d *Daemon) firePending(ctx context.Context, now time.Time) {
	d.mu.Lock()
	var due []string
	for id := range d.pending {
		due = append(due, id)
	}
	d.mu.Unlock()
	for _, id := range due {
		j, err := d.Store.Get(id)
		if err != nil {
			continue
		}
		deb := j.Schedule.Debounce.D()
		if deb <= 0 {
			deb = DefaultDebounce
		}
		d.mu.Lock()
		at, ok := d.pending[id]
		ready := ok && now.Sub(at) >= deb && !d.running[id]
		if ready {
			delete(d.pending, id)
		}
		d.mu.Unlock()
		if ready && j.Status == model.StatusActive && !j.Paused {
			d.start(ctx, j, core.TriggerWatch)
		}
	}
}
