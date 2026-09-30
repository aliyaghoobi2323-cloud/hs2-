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
  old sysctl file removed, with a second (foreign) certbot lineage present — the
  condition that once ended `upgrade` silently with hs2 stopped; an upgrade
  interrupted with Ctrl+C still leaves hs2 running; `hs2 check` on live configs,
  `bash -s manage`
- uninstall → restore → fresh direct tunnel → reboot

Run a subset: `./run.sh install manager`. Transcripts land in `tr_*.txt`.

Quick, no-Docker regression tests:

- `../tests/migrate_test.sh` — the upgrade migration (runs anywhere).
- `../tests/tun_ports_test.py` — the panel inbound over a tun carried by TLS
  (transport tun → tcp): drives the real installer prompts for kharej/iran ×
  direct/reverse through a pty, checks that a masked or silently-not-restarted
  unit is caught instead of reported as running, and — as root, with network
  namespaces — runs the generated configs with the real binary and moves
  traffic from iran's user ports to a panel on kharej.
