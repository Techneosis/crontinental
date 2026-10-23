## Scheduling a command to run later

Anything that has to run on this machine later, or on a repeat, is
**Crontinental** (`crontinental`): load the **`crontinental`** skill first. It
is not the `schedule` skill (cloud routines can't touch this machine), and not
`/loop` or CronCreate (they die with the session). Never hand-write a cron
entry or a `.service`/`.timer` pair, and never use `systemd-run
--on-calendar`, whose transient timer doesn't survive a reboot.
