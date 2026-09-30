# install.sh end-to-end tests

`./run.sh` boots two Ubuntu containers with a real systemd (Iran and Kharej),
installs a reverse mtcp tunnel through the real interactive installer
(`curl | bash`, answers typed by pexpect), and checks 56 behaviours:

- install: service healthy, autostart ON, `hs2-menu` installed, egress from the chosen IP
- tunnel manager: list/details, start/stop/restart, edit (no change, valid,
  broken JSON, semantic error, runtime failure → rollback), live log Ctrl+C, autostart toggle
- reboot: autostart OFF stays down; both servers rebooted come back by themselves,
  including when the egress IP appears only after boot; `kill -9` recovery
- upgrade from an old unit, `hs2 check` on live configs, `bash -s manage`
- uninstall → restore → fresh direct tunnel → reboot

Run a subset: `./run.sh install manager`. Transcripts land in `tr_*.txt`.
