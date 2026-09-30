# Ubuntu 24.04 with a real systemd as PID 1, so the installer, the unit file,
# autostart and reboots behave exactly as on a VPS.
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends systemd systemd-sysv dbus iproute2 iptables \
      curl ca-certificates openssl nano python3 procps kmod less iputils-ping && apt-get clean && rm -rf /var/lib/apt/lists/*
RUN systemctl mask getty@tty1.service console-getty.service systemd-udevd.service 2>/dev/null; true
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
