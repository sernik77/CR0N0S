package cli

import (
	"strings"
	"testing"
	"time"
)

func TestParseCrontab(t *testing.T) {
	tab := `# comment
SHELL=/bin/bash
PATH="/usr/bin:/bin"
MAILTO=me
*/5 * * * *   echo "every five"  >> /tmp/log
@daily  /usr/local/bin/backup --full
@reboot echo boot
0 3 * * 1-5 cd /srv && ./job.sh
bad line
`
	jobs, warnings := ParseCrontab(strings.NewReader(tab))
	if len(jobs) != 3 || len(warnings) != 2 {
		t.Fatalf("jobs=%d warnings=%v", len(jobs), warnings)
	}
	if jobs[0].Schedule.Cron != "*/5 * * * *" || jobs[0].Command != `echo "every five"  >> /tmp/log` {
		t.Errorf("job 0: %q %q", jobs[0].Schedule.Cron, jobs[0].Command)
	}
	if jobs[1].Schedule.Cron != "@daily" || jobs[0].Interpreter != "/bin/bash" || jobs[0].Env["PATH"] != "/usr/bin:/bin" {
		t.Errorf("job 1/env: %+v", jobs[1])
	}
	if _, ok := jobs[0].Env["MAILTO"]; ok {
		t.Error("MAILTO must not leak into the job environment")
	}
}

func TestParseAt(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	if at, _ := parseAt("11:00", now); at.Day() != 11 || at.Hour() != 11 {
		t.Errorf("past HH:MM must roll to tomorrow: %v", at)
	}
	if at, _ := parseAt("2026-12-24 18:00", now); at.Month() != 12 || at.Hour() != 18 {
		t.Errorf("date: %v", at)
	}
	if _, err := parseAt("someday", now); err == nil {
		t.Error("garbage accepted")
	}
}
