#!/bin/bash
# epaper_shutdown_systemd_lab.sh: measures what the shutdown splash's systemd
# design actually does in a REAL shutdown transaction, on the systemd the
# target runs (Debian 12 Bookworm, systemd 252), instead of trusting reasoning
# about ordering. Opt-in: needs Docker (privileged, network for the image
# build) and Go (or Docker, used to build epaperd for the host arch).
# Prints SKIP and exits 0 when Docker is unavailable.
#
#   test/epaper_shutdown_systemd_lab.sh
#
# It runs the REAL debian/stratux_epaper*.service unit files, with a systemd
# as PID 1 in a container, a tmpfs mounted at /boot/firmware holding
# stratux.conf, and the real `epaperd` binary. No display exists in the
# container, so a "drawing" run ends at "could not open GPIO/SPI" - which
# still proves the decision and the timing; the hardware path itself is
# covered by the fake-bus unit tests and, later, by physical acceptance
# (docs/epaper-shutdown-splash.md).
#
# Env: LAB_PARTS="1" runs one part; LAB_KEEP=1 leaves the containers for inspection.
# Part 1 (ordering): the real unit ordering with slow fake renderer bodies.
#   Proves the shutdown ExecStop starts only after the operational renderer
#   AND a boot splash that is still drawing have fully released the panel,
#   that /boot/firmware is still mounted while it runs, and - with a control
#   that removes the boot-splash Before= - that this test can actually fail.
# Part 2 (decision): real epaperd. poweroff and halt draw; reboot, a plain
#   restart, a plain stop and EpaperEnabled=false do not.
set -u
cd "$(dirname "$0")/.."
ROOT=$PWD
fail=0
check() { if [ "$2" = 1 ]; then echo "PASS: $1"; else echo "FAIL: $1"; fail=1; fi; }

command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 || { echo "SKIP: docker not available; real-systemd shutdown lab not run"; exit 0; }
command -v go >/dev/null 2>&1 || GO="docker run --rm -v $ROOT:/src -w /src -e GOFLAGS=-buildvcs=false golang:1.22-bookworm go"
GO=${GO:-go}

WORK=$(mktemp -d); trap '[ -n "${LAB_KEEP:-}" ] || docker rm -f $(docker ps -aq --filter "name=epsdlab-") >/dev/null 2>&1; rm -rf "$WORK"' EXIT
IMG=epsdlab:bookworm
docker build -q -t $IMG - >/dev/null <<'EOF' || { echo "FAIL: could not build the lab image"; exit 1; }
FROM debian:bookworm
RUN apt-get update && apt-get install -y --no-install-recommends systemd systemd-sysv dbus procps util-linux && rm -rf /var/lib/apt/lists/* \
 && systemctl mask systemd-logind.service getty.target console-getty.service systemd-udevd.service systemd-remount-fs.service \
 && mkdir -p /var/log/journal /boot/firmware /opt/stratux/bin /lab
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
EOF
$GO build -o "$WORK/epaperd" ./epaper_main/ || { echo "FAIL: could not build epaperd"; exit 1; }

# --- lab-only support units and scripts ------------------------------------
mkdir -p "$WORK/lab" "$WORK/units"
cat > "$WORK/units/boot-firmware.mount" <<'EOF'
[Unit]
Description=lab stand-in for the /boot/firmware mount
[Mount]
What=tmpfs
Where=/boot/firmware
Type=tmpfs
[Install]
WantedBy=local-fs.target
EOF
cat > "$WORK/units/seedconf.service" <<'EOF'
[Unit]
Description=lab: put stratux.conf on the /boot/firmware stand-in
RequiresMountsFor=/boot/firmware
Before=stratux_epaper_splash.service stratux_epaper_shutdown.service stratux_epaper.service stratux.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/cp /lab/stratux.conf /boot/firmware/stratux.conf
[Install]
WantedBy=multi-user.target
EOF
cat > "$WORK/units/stratux.service" <<'EOF'
[Unit]
Description=lab stub of the Stratux daemon
[Service]
ExecStart=/bin/sleep infinity
[Install]
WantedBy=multi-user.target
EOF
# Part 1 fakes: log timestamped events, hold the "panel" for a while.
cat > "$WORK/lab/log.sh" <<'EOF'
#!/bin/bash
mp=no; mountpoint -q /boot/firmware && mp=yes
printf '%s %s %s mount=%s\n' "$(date +%s.%N)" "$1" "$2" "$mp" >> /var/log/lab-events.log
EOF
cat > "$WORK/lab/hold.sh" <<'EOF'
#!/bin/bash
# hold.sh NAME RUN_SECONDS RELEASE_SECONDS: "owns the panel" while it runs; on SIGTERM it takes RELEASE_SECONDS to let go
/lab/log.sh "$1" acquired
trap '/lab/log.sh "$1" got-TERM; sleep "${3:-0}"; /lab/log.sh "$1" released; exit 0' TERM
sleep "$2" & wait $!
/lab/log.sh "$1" released
EOF
chmod +x "$WORK"/lab/*.sh

# new_lab NAME CONF_JSON: create (not start) a container with the real unit
# files, the real epaperd, the lab support units and the given stratux.conf.
new_lab() {
	local n=epsdlab-$1 conf=$2
	docker rm -f "$n" >/dev/null 2>&1
	docker create --name "$n" --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw $IMG /sbin/init >/dev/null || return 1
	printf '%s' "$conf" > "$WORK/stratux.conf"
	docker cp "$WORK/epaperd" "$n:/opt/stratux/bin/epaperd"
	docker cp "$WORK/lab/." "$n:/lab/"
	docker cp "$WORK/stratux.conf" "$n:/lab/stratux.conf"
	docker cp "$WORK/units/." "$n:/etc/systemd/system/"
	for u in stratux_epaper stratux_epaper_splash stratux_epaper_shutdown; do docker cp "$ROOT/debian/$u.service" "$n:/etc/systemd/system/$u.service"; done
	docker start "$n" >/dev/null
	for _ in $(seq 1 30); do docker exec "$n" systemctl is-system-running 2>/dev/null | grep -qE 'running|degraded' && break; sleep 1; done
	docker exec "$n" bash -c 'systemctl daemon-reload; systemctl enable boot-firmware.mount seedconf stratux stratux_epaper stratux_epaper_splash stratux_epaper_shutdown >/dev/null 2>&1; systemctl start multi-user.target'
	sleep 2
}
# journal NAME: the whole journal of a (running or stopped) lab container.
journal() { # NAME [journalctl args...]
	local n=epsdlab-$1 d=$WORK/j-$1; shift
	rm -rf "$d"; mkdir -p "$d"
	docker exec "$n" journalctl --flush >/dev/null 2>&1
	docker cp "$n:/var/log/journal/." "$d/" >/dev/null 2>&1
	journalctl --directory="$d" -o short-precise --no-pager "$@" 2>/dev/null
}
wait_stopped() { for _ in $(seq 1 90); do [ "$(docker inspect -f '{{.State.Running}}' "epsdlab-$1")" = false ] && return 0; sleep 1; done; return 1; }
lineno() { grep -n -m1 -- "$2" "$1" | cut -d: -f1; }

part1() {
echo "=== Part 1: ordering (real unit files, slow fake renderers) ==="
# ord_case NAME real|control op|boot
#  op:   the operational renderer owns the panel (the boot splash has finished)
#        when power-off arrives; it takes 3 s to release after SIGTERM.
#  boot: the boot splash is still drawing (the operational renderer is queued
#        behind it, as at a real boot); it takes 3 s to release after SIGTERM.
# The shutdown renderer is a 1 s fake. `control` removes the boot-splash
# Before= from the shutdown unit, to show this test can fail.
ord_case() {
	local name=ord-$1-$2 variant=$2 mode=$3
	new_lab $name '{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2"}'
	docker exec epsdlab-$name bash -c '
		mkdir -p /etc/systemd/system/stratux_epaper{,_splash,_shutdown}.service.d
		printf "[Service]\nExecStart=\nExecStart=/lab/hold.sh operational 1000 3\n" > /etc/systemd/system/stratux_epaper.service.d/lab.conf
		printf "[Service]\nExecStop=\nExecStop=/lab/hold.sh shutdownsplash 1 0\n" > /etc/systemd/system/stratux_epaper_shutdown.service.d/lab.conf'
	if [ $mode = op ]; then
		docker exec epsdlab-$name bash -c 'printf "[Service]\nExecStart=\nExecStart=/lab/hold.sh bootsplash 1 0\n" > /etc/systemd/system/stratux_epaper_splash.service.d/lab.conf'
	else
		docker exec epsdlab-$name bash -c 'printf "[Service]\nExecStart=\nExecStart=/lab/hold.sh bootsplash 1000 3\nTimeoutStartSec=infinity\n" > /etc/systemd/system/stratux_epaper_splash.service.d/lab.conf'
	fi
	# (An empty Before= in a drop-in does NOT reset the list in systemd 252, so the control edits the unit copy.)
	[ $variant = control ] && docker exec epsdlab-$name sed -i 's/^Before=.*/Before=stratux_epaper.service/' /etc/systemd/system/stratux_epaper_shutdown.service
	docker exec epsdlab-$name bash -c 'systemctl daemon-reload; : > /var/log/lab-events.log'
	if [ $mode = op ]; then
		docker exec epsdlab-$name bash -c 'systemctl restart stratux_epaper_splash && systemctl restart stratux_epaper; sleep 1; systemctl poweroff' >/dev/null 2>&1
	else
		docker exec epsdlab-$name bash -c 'systemctl restart --no-block stratux_epaper_splash stratux_epaper; sleep 2; systemctl poweroff' >/dev/null 2>&1
	fi
	wait_stopped $name || echo "WARN: $name did not stop"
	docker cp epsdlab-$name:/var/log/lab-events.log "$WORK/ev-$name.log" >/dev/null 2>&1
	journal $name > "$WORK/jn-$name.txt"
	echo "--- events ($variant, $mode):"; awk 'NR==1{t0=$1} {printf "  +%6.2f %s %s %s\n",$1-t0,$2,$3,$4}' "$WORK/ev-$name.log"
	local holder=operational; [ $mode = boot ] && holder=bootsplash
	local rel acq; rel=$(awk -v h=$holder '$2==h&&$3=="released"{print $1}' "$WORK/ev-$name.log" | tail -1); acq=$(awk '$2=="shutdownsplash"&&$3=="acquired"{print $1}' "$WORK/ev-$name.log" | tail -1)
	[ -n "$rel" ] && [ -n "$acq" ] || { check "$name: events recorded ($holder released, shutdownsplash acquired)" 0; return; }
	local after; after=$(awk -v a="$acq" -v b="$rel" 'BEGIN{print (a>=b)?1:0}')
	case $variant in
	real)
		check "power-off with the $holder renderer holding the panel: the shutdown renderer acquires only after it has released" "$after"
		check "...and /boot/firmware is still mounted for the whole shutdown-renderer run" "$(grep -q 'shutdownsplash acquired mount=yes' "$WORK/ev-$name.log" && grep -q 'shutdownsplash released mount=yes' "$WORK/ev-$name.log" && echo 1 || echo 0)"
		local u st; u=$(lineno "$WORK/jn-$name.txt" 'Unmounting boot-firmware.mount'); st=$(lineno "$WORK/jn-$name.txt" 'Stopped stratux_epaper_shutdown.service')
		check "...and /boot/firmware is unmounted only after the shutdown unit has stopped" "$([ -n "$u" ] && [ -n "$st" ] && [ "$u" -gt "$st" ] && echo 1 || echo 0)";;
	control)
		check "control (no Before=stratux_epaper_splash.service, $holder holding): the overlap IS produced, so this test can fail" "$([ "$after" = 0 ] && echo 1 || echo 0)";;
	esac
}
ord_case a real op
ord_case b real boot
ord_case c control boot

}
part2() {
echo "=== Part 2: decision (real unit files, real epaperd) ==="
ENABLED='{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":0}'
decide() { # NAME CONF ACTION EXPECT(draw|skip:<text>|none) DESCRIPTION
	local name=dec-$1 conf=$2 action=$3 expect=$4 desc=$5
	new_lab $name "$conf"
	docker exec epsdlab-$name bash -c "$action" >/dev/null 2>&1
	if [ "$(docker inspect -f '{{.State.Running}}' epsdlab-$name)" = true ]; then sleep 4; else wait_stopped $name; fi
	# Only what the shutdown unit's own epaperd printed, and the whole journal for ordering.
	local all=$WORK/ja-$name.txt own=$WORK/jo-$name.txt
	journal $name > "$all"; journal $name -u stratux_epaper_shutdown.service | grep 'epaperd\[' > "$own"
	case $expect in
	draw)
		check "$desc: draws (power-off detected, render attempted)" "$(grep -q 'shutdown splash: power-off in progress' "$own" && grep -q 'could not open GPIO/SPI' "$own" && echo 1 || echo 0)"
		a=$(lineno "$all" 'Stopped stratux_epaper.service'); b=$(lineno "$all" 'epaperd.*shutdown splash: power-off in progress')
		check "$desc: the operational renderer had fully stopped before the shutdown renderer ran" "$([ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ] && echo 1 || echo 0)";;
	skip:*)
		check "$desc: does not draw ('${expect#skip:}')" "$(grep -q "shutdown splash skipped: .*${expect#skip:}" "$own" && ! grep -q 'power-off in progress\|could not open GPIO/SPI' "$own" && echo 1 || echo 0)";;
	none)
		check "$desc: the shutdown unit did not run at all" "$([ ! -s "$own" ] && echo 1 || echo 0)";;
	esac
}
decide poweroff "$ENABLED" 'systemctl poweroff' draw "systemctl poweroff"
decide halt "$ENABLED" 'systemctl halt' draw "systemctl halt"
decide shutdown_h "$ENABLED" 'shutdown -h now' draw "shutdown -h now"
decide reboot "$ENABLED" 'systemctl reboot' 'skip:the system is rebooting' "systemctl reboot"
decide shutdown_r "$ENABLED" 'shutdown -r now' 'skip:the system is rebooting' "shutdown -r now"
decide disabled '{"EpaperEnabled":false}' 'systemctl poweroff' 'skip:EpaperEnabled is false' "poweroff with EpaperEnabled=false"
decide panel37 '{"EpaperEnabled":true,"EpaperPanel":"waveshare-3.7in"}' 'systemctl poweroff' 'skip:waveshare-3.7in' "poweroff with an unsupported panel"
decide restart_op "$ENABLED" 'systemctl restart stratux_epaper' none "systemctl restart stratux_epaper (ordinary service restart)"
decide pkg_stop "$ENABLED" 'systemctl stop stratux_epaper_splash; systemctl stop stratux_epaper; systemctl daemon-reload' none "prerm-style stop of the renderers + daemon-reload"
decide stop_self "$ENABLED" 'systemctl stop stratux_epaper_shutdown' 'skip:not a system power-off' "systemctl stop stratux_epaper_shutdown (prerm)"
decide restart_self "$ENABLED" 'systemctl restart stratux_epaper_shutdown' 'skip:not a system power-off' "systemctl restart stratux_epaper_shutdown"
}
for p in ${LAB_PARTS:-1 2}; do part$p; done
exit $fail
