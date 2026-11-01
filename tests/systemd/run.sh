#!/bin/sh
# Runs the systemd tests in a container that boots systemd: install, update
# and uninstall of the service, in the system scope as root and in the user
# scope as an unprivileged user. Needs docker; set DOCKER="sudo docker" if your
# user may not use it directly.
set -eu
docker=${DOCKER:-docker}
cd "$(dirname "$0")/../.."
pkg=github.com/exa-pub/grokbot-tunnel/internal/api/cli
out=$(mktemp -d)
cid=
cleanup() {
	[ -z "$cid" ] || $docker rm -f "$cid" >/dev/null
	rm -rf "$out"
}
trap cleanup EXIT
CGO_ENABLED=0 go build -ldflags "-X $pkg.version=v1.0.0" -o "$out/grokbot-tunnel" ./cmd/grokbot-tunnel
CGO_ENABLED=0 go build -ldflags "-X $pkg.version=v9.9.9" -o "$out/grokbot-tunnel-release" ./cmd/grokbot-tunnel
CGO_ENABLED=0 go test -c -tags systemd -o "$out/systemd.test" ./tests/systemd
chmod -R a+rX "$out"
$docker build -q -t grokbot-tunnel-systemd tests/systemd >/dev/null
cid=$($docker run -d --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$out:/work:ro" grokbot-tunnel-systemd)
# Wait for boot; "degraded" is fine, as some units of a minimal image fail in
# a container. systemctl fails until the bus is up.
for _ in $(seq 60); do
	state=$($docker exec "$cid" systemctl is-system-running 2>/dev/null) || true
	case $state in running | degraded) break ;; esac
	sleep 1
done
$docker exec -e GROK_BIN=/work/grokbot-tunnel -e GROK_RELEASE_BIN=/work/grokbot-tunnel-release \
	"$cid" /work/systemd.test -test.v -test.timeout 5m "$@"
