package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sernik77/CR0N0S/internal/model"
)

func newJob() *model.Job {
	return &model.Job{Title: "t", Path: "/tmp", Command: "true", Armed: true,
		Schedule: model.Schedule{Kind: model.ScheduleOnce, At: time.Now().Add(time.Hour)}}
}

func open(t *testing.T) *Store {
	st, err := Open(filepath.Join(t.TempDir(), "cr0.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRegisterGetUpdate(t *testing.T) {
	st := open(t)
	j := newJob()
	if err := st.Register(j); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(j.UUID[:6])
	if err != nil || got.Title != "t" {
		t.Fatalf("get by prefix: %v %v", got, err)
	}
	if _, err := st.Update(j.UUID, Audit{Action: "modify"}, func(j *model.Job) error { j.Title = "x"; return nil }); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("modify without reason: %v", err)
	}
	if _, err := st.Update(j.UUID, Audit{Action: "modify", Reason: "rename", ReasonDesc: "long"}, func(j *model.Job) error { j.Title = "x"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(j.UUID, Audit{Action: "modify", Reason: "bad"}, func(j *model.Job) error { j.Path = "relative"; return nil }); err == nil {
		t.Fatal("invalid spec saved")
	}
	revs, _ := st.Revisions(j.UUID, 0)
	if len(revs) != 2 || revs[0].Reason != "rename" || revs[0].ReasonDesc != "long" || revs[0].Before == "" {
		t.Fatalf("revisions %+v", revs)
	}
	if got, _ := st.Get(j.UUID); got.Title != "x" {
		t.Fatal("update not saved")
	}
}

func TestRunsAndTemplates(t *testing.T) {
	st := open(t)
	j := newJob()
	st.Register(j)
	now := time.Now()
	st.AddRun(&model.Run{JobUUID: j.UUID, Trigger: "manual", StartedAt: now, FinishedAt: now, Status: "success"})
	if runs, _ := st.Runs(j.UUID, 0); len(runs) != 1 || runs[0].Status != "success" {
		t.Fatalf("runs %+v", runs)
	}
	if err := st.SaveTemplate(&model.Template{Name: "n", Spec: *j}); err != nil {
		t.Fatal(err)
	}
	if tp, err := st.Template("n"); err != nil || tp.Spec.Command != "true" {
		t.Fatalf("template %v %v", tp, err)
	}
	if err := st.DeleteTemplate("n"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Template("n"); !errors.Is(err, ErrNotFound) {
		t.Fatal("template not deleted")
	}
}
