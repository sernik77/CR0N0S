package cond

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sernik77/CR0N0S/internal/model"
)

func TestParseAndEvaluate(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), nil, 0o644)
	t.Setenv("CR0_TEST", "yes")
	noon := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local) // a Tuesday
	env := Env{Now: noon, Dir: dir, JobStatus: func(id string) (string, error) { return "success", nil }}

	cases := map[string]bool{
		"file:a": true, "file:b": false, "!file:b": true, "dir:.": true,
		"env:CR0_TEST": true, "env:CR0_TEST=yes": true, "env:CR0_TEST=no": false,
		"time:11:00-13:00": true, "time:22:00-06:00": false, "days:mon,tue": true, "days:sat": false,
		"cmd:true": true, "cmd:false": false, "job:abcd": true, "job:abcd=fail": false,
	}
	for in, want := range cases {
		c, err := Parse(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := Evaluate(&c, env).Met; got != want {
			t.Errorf("%s: got %v want %v", in, got, want)
		}
	}
	if _, err := Parse("bogus:x"); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestWindowWrapsMidnight(t *testing.T) {
	late := time.Date(2026, 1, 1, 23, 30, 0, 0, time.Local)
	if ok, _ := inWindow(late, "22:00", "06:00"); !ok {
		t.Error("23:30 should be inside 22:00-06:00")
	}
}

func TestSummarize(t *testing.T) {
	c := &model.Condition{All: []model.Condition{
		{Type: "env", Name: "CR0_NOPE_UNSET"},
		{Any: []model.Condition{{Type: "command", Command: "true"}, {Type: "job_status", Job: "dep1", Status: "success"}}},
	}}
	s := Summarize(c, Env{Now: time.Now(), JobStatus: func(string) (string, error) { return "fail", nil }})
	if s.Count != 3 || s.MetCount != 1 || s.Met {
		t.Fatalf("summary %+v", s)
	}
	if len(s.DependsOn) != 1 || s.DependsOn[0] != "dep1" {
		t.Fatalf("depends %v", s.DependsOn)
	}
	neg := &model.Condition{Not: &model.Condition{Type: "env", Name: "CR0_NOPE_UNSET"}}
	if s := Summarize(neg, Env{}); !s.Met || s.Count != 1 || s.MetCount != 1 {
		t.Fatalf("negated leaf must count as satisfied: %+v", s)
	}
	if s := Summarize(nil, Env{}); !s.Met || s.Count != 0 {
		t.Fatal("nil conditions must be met")
	}
}

func TestPathChanged(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	os.WriteFile(f, nil, 0o644)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	c := &model.Condition{Type: "path_changed", Path: f}
	if !Evaluate(c, Env{LastRun: &past}).Met || Evaluate(c, Env{LastRun: &future}).Met {
		t.Fatal("path_changed")
	}
}
