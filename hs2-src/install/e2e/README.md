# install.sh end-to-end tests

`./run.sh` boots two Ubuntu containers with a real systemd (Iran and Kharej),
installs a reverse mtcp tunnel through the real interactive installer
(`curl | bash`, answers typed by pexpect), and checks ~70 behaviours:

- install: service healthy, autostart ON, `hs2-menu` installed, egress from the
  chosen IP, unit has ExecReload, no static sysctl file (runtime tuning), bbr module persisted
- tunnel manager: list/details (live Pattern + cert days), start/stop/restart,
  edit (no change, valid, broken JSON, semantic error, runtime failure →
  rollback), live log Ctrl+C, autostart toggle
- adaptive: `hs2 status` live pattern, the status file on tmpfs, `hs2 tune`
  profile, the manager's Tuning screen (change qdisc, applied, still healthy)
- reboot: autostart OFF stays down; both servers rebooted come back by themselves,
  including when the egress IP appears only after boot; `kill -9` recovery
- upgrade from an old unit + fixed link pool → migrated to the 2–32 envelope and
  old sysctl file removed; `hs2 check` on live configs, `bash -s manage`
- uninstall → restore → fresh direct tunnel → reboot

Run a subset: `./run.sh install manager`. Transcripts land in `tr_*.txt`.
