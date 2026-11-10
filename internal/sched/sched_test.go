package sched

import (
	"testing"
	"time"

	"github.com/sernik77/CR0N0S/internal/model"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func TestNextCron(t *testing.T) {
	j := &model.Job{Schedule: model.Schedule{Kind: model.ScheduleCron, Cron: "30 3 * * *"}}
	n, err := Next(j, t0)
	if err != nil || !n.Equal(time.Date(2026, 11, 11, 3, 30, 0, 0, time.UTC)) {
		t.Fatalf("got %v %v", n, err)
	}
	j.Schedule.Cron = "*/10 * * * * *" // seconds field
	if n, _ = Next(j, t0); !n.Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("seconds cron: %v", n)
	}
	if _, err := ParseCron("nope"); err == nil {
		t.Fatal("invalid cron accepted")
	}
}

func TestNextInterval(t *testing.T) {
	j := &model.Job{RegisteredAt: t0, Schedule: model.Schedule{Kind: model.ScheduleInterval, Every: model.Duration(time.Hour)}}
	n, _ := Next(j, t0)
	if !n.Equal(t0.Add(time.Hour)) {
		t.Fatalf("first: %v", n)
	}
	// Daemon was down for a while: the next slot stays aligned to the base.
	n, _ = Next(j, t0.Add(5*time.Hour+time.Minute))
	if !n.Equal(t0.Add(6 * time.Hour)) {
		t.Fatalf("aligned: %v", n)
	}
}

func TestNextOnce(t *testing.T) {
	j := &model.Job{Schedule: model.Schedule{Kind: model.ScheduleOnce, At: t0}}
	if n, _ := Next(j, t0.Add(-time.Hour)); !n.Equal(t0) {
		t.Fatalf("once: %v", n)
	}
	j.LastRun = &t0
	if n, _ := Next(j, t0); n != nil {
		t.Fatalf("once after run: %v", n)
	}
	if n, _ := Next(&model.Job{Schedule: model.Schedule{Kind: model.ScheduleManual}}, t0); n != nil {
		t.Fatal("manual has no next run")
	}
}

func TestPauseResumeShiftsOnce(t *testing.T) {
	at := t0.Add(time.Hour)
	j := &model.Job{Schedule: model.Schedule{Kind: model.ScheduleOnce, At: at}, NextRun: &at}
	Pause(j, t0)
	if err := Resume(j, t0.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	want := at.Add(20 * time.Minute)
	if j.Paused || !j.Schedule.At.Equal(want) || !j.NextRun.Equal(want) {
		t.Fatalf("resume: paused=%v at=%v next=%v", j.Paused, j.Schedule.At, j.NextRun)
	}
}

func TestResumeRecurringRecomputes(t *testing.T) {
	j := &model.Job{Schedule: model.Schedule{Kind: model.ScheduleCron, Cron: "0 * * * *"}}
	Pause(j, t0)
	Resume(j, t0.Add(3*time.Hour+time.Minute))
	if !j.NextRun.Equal(t0.Add(4 * time.Hour)) {
		t.Fatalf("next %v", j.NextRun)
	}
}
