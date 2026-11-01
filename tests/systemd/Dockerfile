# A container that boots systemd, for run.sh.
FROM ubuntu:24.04
RUN apt-get update \
	&& apt-get install -y --no-install-recommends systemd systemd-sysv libpam-systemd dbus ca-certificates \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd -m -s /bin/bash tester
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
