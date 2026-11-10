# CR0N0S

`cr0` is a better cron: a dynamic job scheduler, manager, tracker and templater
with conditions, hooks, reminders and a full history ledger. It is a single Go
binary with an embedded SQLite database (no cgo), a systemd service, cronie
import/export and an interactive TUI.

- **Schedules**: once (`--at`, `--in`), cron expressions (5/6 fields, `@daily`),
  fixed intervals (`--every`), file watching via inotify (`--watch`), or manual.
- **Any interpreter**: `sh bash zsh dash ksh fish pwsh python node php ruby perl`,
  inline commands or scripts, arguments, environment, working directory, timeout.
- **Conditions**: a declarative tree (`all` / `any` / `not`) of file, directory,
  path-changed, command, env, time-window, weekday, other-job-status and
  process checks. Control flow with `if` / `elif` / `else` / `while` steps.
- **Authority levels**: run as yourself, as another user (system daemon), or as
  superuser — either pre-authorized (system daemon) or authorized with `pkexec`
  at execution time.
- **State**: arm/disarm (execution on/off; the timer keeps running and runs are
  recorded as skipped), pause/resume (timer on/off; one-shot jobs are shifted
  by the paused time), reschedule, modify, cancel.
- **Audit**: every modification requires `--reason` (and optionally
  `--reason-desc`) and is stored with before/after snapshots.
- **Hooks** on `success` / `fail` / `error` / `unknown`: desktop notification,
  webhook (JSON POST) or shell command. Reminders before execution.
- **Templates**: save common jobs and prefill new ones; flags override fields.
- **Inspector**: per job, the number of conditions, how many are met and whether
  the conditions are met — evaluated for the next execution time, assuming
  nothing else changes. Execution time itself is not counted as a condition.

## Install

```sh
make install            # builds ./cr0 and copies it to ~/.local/bin
cr0 service install     # writes the systemd user unit
systemctl --user daemon-reload && systemctl --user enable --now cr0.service
```

For jobs that run as other users or as pre-authorized superuser, run the system
daemon instead (`cr0 --system service install` prints the steps). Without
systemd, cronie can drive the scheduler: `cr0 cron export --tick` prints
`* * * * * cr0 daemon --once`.

## Usage

`cr0` alone prints help. `cr0 <command> --help` documents every flag.

```sh
# one-shot, in 25 minutes, with a reminder 5 minutes before
cr0 job register -t tea --in 25m --remind 5m -c 'notify-send "tea is ready"'

# nightly backup, only when the disk is mounted, notify on failure
cr0 job register -t backup --cron "0 3 * * *" --shell bash \
  -c "restic -r /mnt/backup backup ~/docs" -w dir:/mnt/backup \
  --on-fail notify:"backup failed (exit {exit})" --timeout 2h

# rebuild whenever src/ changes, only on a clean git tree
cr0 job register -t build --watch src --events write -p ~/proj -c make -w cmd:"git diff --quiet"

# run a script as superuser, asking pkexec at execution time
cr0 job register -t updates --cron "0 12 * * sun" --sudo --sudo-mode exec -s /usr/local/bin/update.sh

# from a spec file (TOML or JSON); flags override its fields
cr0 job register -f examples/backup.toml -p /srv/data
```

Browse and manage:

```sh
cr0 jobs                              # quick list: what, where, when, until, registered
cr0 job list --future --here          # filters: --all --past --future --here --path --command --status --tag
cr0 job inspect 3fa8                  # details, condition tree, last runs, revisions (UUID prefix is enough)
cr0 job history [3fa8] [--revisions]  # run ledger / audit trail
cr0 job modify 3fa8 -r "slower disk" --reason-desc "NAS moved" --timeout 3h
cr0 job reschedule 3fa8 -r "moved window" --cron "0 4 * * *"
cr0 job disarm 3fa8 | arm | pause | resume | cancel
cr0 job run 3fa8                      # run now in the foreground
cr0 job export 3fa8 > job.toml
cr0 ui                                # interactive TUI
```

Templates:

```sh
cr0 template add nightly --cron "0 2 * * *" --shell bash --timeout 1h --on-fail notify:"{title} failed"
cr0 job register -T nightly -t "db dump" -p /srv/db -c "./dump.sh"
cr0 template add deploy --from 3fa8   # from an existing job
```

cronie:

```sh
cr0 cron import --dry-run             # from `crontab -l` (or -f FILE)
cr0 cron export                       # cron jobs as crontab lines
```

### Condition shorthand

`--when` (all must hold, repeatable) and `--when-any` (at least one) accept:

| shorthand | meaning |
| --- | --- |
| `file:PATH` / `dir:PATH` | file / directory exists (relative to the job path) |
| `changed:PATH` | modified since the job's last run |
| `cmd:COMMAND` | command exits 0 (10 s timeout) |
| `env:NAME[=VALUE]` | variable set (and equal) |
| `time:HH:MM-HH:MM` | inside a daily window (may wrap midnight) |
| `days:mon,fri` | on these weekdays |
| `job:ID[=STATUS]` | another job's last run status (default `success`) |
| `proc:NAME` | a process with that name is running |

Prefix with `!` to negate. Nested trees and `if`/`elif`/`else`/`while` logic are
written in spec files — see [`examples/backup.toml`](examples/backup.toml).

### Hook messages

Notification messages may use `{title}`, `{uuid}`, `{status}`, `{exit}` and
`{error}`. Hook commands get `CR0_JOB_UUID`, `CR0_JOB_TITLE`, `CR0_STATUS`,
`CR0_EXIT_CODE` and `CR0_ERROR`; jobs themselves get `CR0_JOB_UUID` and
`CR0_JOB_TITLE`.

## Data

The database lives at `$CR0_DB`, else `~/.local/share/cr0/cr0.db` for users and
`/var/lib/cr0/cr0.db` for root / `--system`. Override per call with `--db`.
Every listing supports `--json`.

## Notes and limits

- The CLI writes the database directly; the daemon polls it every second.
- `notify-send` from the root daemon usually cannot reach a user session.
- `--sudo-mode exec` needs a polkit agent at execution time. A systemd user
  unit may not see your graphical session's agent (untested); for unattended
  superuser jobs use `--sudo-mode preauth` with the system daemon.
- The system daemon runs `/usr/local/bin/cr0` (root-owned), never a copy in
  your home. In the system scope every job needs a user (defaults to the user
  who ran `pkexec`/`sudo`) unless it is a superuser job.
- `cmd:` condition probes run as the daemon's user (root for the system daemon).
- A one-shot job whose conditions are not met at its execution time is
  recorded as skipped and finished.
- Missed runs (daemon down) execute once on start (`catchup = "once"`) or are
  recorded as missed (`catchup = "skip"`).

## License

MIT — see [LICENSE](LICENSE).
