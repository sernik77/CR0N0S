package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sernik77/CR0N0S/internal/model"
	"github.com/sernik77/CR0N0S/internal/runner"
	"github.com/sernik77/CR0N0S/internal/store"
)

func setup(t *testing.T) (*Daemon, *store.Store, string) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "cr0.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, runner.New(), log.New(io.Discard, "", 0)), st, dir
}

func register(t *testing.T, st *store.Store, j *model.Job) *model.Job {
	t.Helper()
	if j.NextRun == nil && j.Schedule.Kind == model.ScheduleOnce {
		at := j.Schedule.At
		j.NextRun = &at
	}
	if err := st.Register(j); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestOnceRunsAndCompletes(t *testing.T) {
	d, st, dir := setup(t)
	at := time.Now().Add(-time.Second)
	j := register(t, st, &model.Job{Title: "once", Path: dir, Command: "touch ran", Armed: true,
		Schedule: model.Schedule{Kind: model.ScheduleOnce, At: at}})
	d.Tick(context.Background(), time.Now())
	d.Wait()
	d.Tick(context.Background(), time.Now()) // must not fire twice
	d.Wait()
	if _, err := os.Stat(filepath.Join(dir, "ran")); err != nil {
		t.Fatal("job did not run")
	}
	runs, _ := st.Runs(j.UUID, 0)
	got, _ := st.Get(j.UUID)
	if len(runs) != 1 || got.Status != model.StatusDone || got.LastStatus != model.RunSuccess {
		t.Fatalf("runs=%d status=%s last=%s", len(runs), got.Status, got.LastStatus)
	}
}

func TestDisarmedSkipsPausedWaits(t *testing.T) {
	d, st, dir := setup(t)
	at := time.Now().Add(-time.Second)
	dis := register(t, st, &model.Job{Title: "dis", Path: dir, Command: "touch dis", Armed: false,
		Schedule: model.Schedule{Kind: model.ScheduleOnce, At: at}})
	pau := register(t, st, &model.Job{Title: "pau", Path: dir, Command: "touch pau", Armed: true, Paused: true,
		Schedule: model.Schedule{Kind: model.ScheduleOnce, At: at}})
	d.Tick(context.Background(), time.Now())
	d.Wait()
	if runs, _ := st.Runs(dis.UUID, 0); len(runs) != 1 || runs[0].Status != model.RunSkipped {
		t.Fatalf("disarmed: %+v", runs)
	}
	if runs, _ := st.Runs(pau.UUID, 0); len(runs) != 0 {
		t.Fatalf("paused job ran: %+v", runs)
	}
	for _, f := range []string{"dis", "pau"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s executed", f)
		}
	}
}

func TestCatchupSkip(t *testing.T) {
	d, st, dir := setup(t)
	at := time.Now().Add(-time.Hour)
	j := register(t, st, &model.Job{Title: "late", Path: dir, Command: "touch late", Armed: true, Catchup: model.CatchupSkip,
		Schedule: model.Schedule{Kind: model.ScheduleOnce, At: at}})
	d.Tick(context.Background(), time.Now())
	d.Wait()
	runs, _ := st.Runs(j.UUID, 0)
	if len(runs) != 1 || runs[0].Status != model.RunMissed {
		t.Fatalf("catchup skip: %+v", runs)
	}
}

func TestWatchTriggers(t *testing.T) {
	d, st, dir := setup(t)
	watched := filepath.Join(dir, "in")
	os.Mkdir(watched, 0o755)
	j := register(t, st, &model.Job{Title: "w", Path: dir, Command: "touch fired", Armed: true,
		Schedule: model.Schedule{Kind: model.ScheduleWatch, Watch: []string{"in"}, Events: []string{"create"},
			Debounce: model.Duration(50 * time.Millisecond)}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }() // stop the daemon before the store closes
	d.Poll = 20 * time.Millisecond
	go func() { d.Run(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	os.WriteFile(filepath.Join(watched, "new"), []byte("x"), 0o644)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runs, _ := st.Runs(j.UUID, 0); len(runs) > 0 {
			if runs[0].Trigger != "watch" || runs[0].Status != model.RunSuccess {
				t.Fatalf("watch run %+v", runs[0])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("watch job did not fire")
}
