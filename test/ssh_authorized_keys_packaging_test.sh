#!/bin/bash
# ssh_authorized_keys_packaging_test.sh: proves the persistent-SSH-key restore
# unit and its packaging are wired correctly and can never block sshd.
# Parses the REAL files that ship (never copies), executes the real maintainer
# scripts against stubs, and asks the real systemd parser to verify the unit.
set -u
cd "$(dirname "$0")/.." || exit 1

fail=0
check() { if [ "$2" = 1 ]; then echo "PASS: $1"; else echo "FAIL: $1"; fail=1; fi; }

UNIT=debian/stratux_ssh_authorized_keys.service
HELPER=debian/stratux-ssh-authorized-keys.sh
POSTINST=debian/postinst.dpkg
PRERM=debian/prerm.dpkg
NAME=stratux_ssh_authorized_keys
INSTALLED=/opt/stratux/bin/stratux-ssh-authorized-keys.sh

noncomment() { grep -v -E '^\s*(#|;)' "$1" | grep -v '^\s*$'; }
val() { noncomment "$UNIT" | grep -E "^$1=" | head -1 | cut -d= -f2-; }
has() { noncomment "$UNIT" | grep -q -E "$1" && echo 1 || echo 0; }

# --- The unit's structure ---------------------------------------------------
check "unit and helper files exist and the helper is executable" "$([ -f "$UNIT" ] && [ -x "$HELPER" ] && echo 1 || echo 0)"
check "Type=oneshot with RemainAfterExit=yes (so a restart re-applies an edited key file)" "$([ "$(val Type)" = oneshot ] && [ "$(val RemainAfterExit)" = yes ] && echo 1 || echo 0)"
check "ExecStart, ConditionFileIsExecutable and the Makefile install path all name the same helper" "$([ "$(val ExecStart)" = "$INSTALLED" ] && [ "$(val ConditionFileIsExecutable)" = "$INSTALLED" ] && grep -q 'cp -f debian/stratux-ssh-authorized-keys.sh $(STRATUX_HOME)/bin/' Makefile && grep -q '^export STRATUX_HOME := /opt/stratux/$' Makefile && echo 1 || echo 0)"
check "runs before ssh.service" "$(noncomment "$UNIT" | grep -E '^Before=' | grep -q -w 'ssh.service' && echo 1 || echo 0)"
check "ordered after the data partition's mount unit (var-lib-stratux\\x2ddata.mount)" "$(noncomment "$UNIT" | grep -E '^After=' | grep -q -F 'var-lib-stratux\x2ddata.mount' && echo 1 || echo 0)"
check "runs only when /var/lib/stratux-data is really a mount point" "$([ "$(val ConditionPathIsMountPoint)" = /var/lib/stratux-data ] && echo 1 || echo 0)"
check "nothing else is ordered: After= names only the data mount (never ssh)" "$([ "$(noncomment "$UNIT" | grep -E '^After=' | tr -s ' =' '\n' | grep -v -E '^After$' | wc -l)" = 1 ] && ! noncomment "$UNIT" | grep -E '^After=' | grep -q -i ssh && echo 1 || echo 0)"
check "no Requires/Requisite/BindsTo/PartOf/Wants/Upholds/Conflicts/OnFailure in the unit (cannot block or fail sshd)" "$(has '^(Requires|Requisite|BindsTo|PartOf|Wants|Upholds|Conflicts|OnFailure|OnSuccess|RequiresMountsFor|PropagatesStopTo)=' | sed 's/1/0/;t;s/0/1/')"
check "DefaultDependencies is not disabled (stopped normally at shutdown)" "$(has '^DefaultDependencies=no' | sed 's/1/0/;t;s/0/1/')"
check "enabled via WantedBy=multi-user.target only" "$([ "$(noncomment "$UNIT" | grep -c '^WantedBy=')" = 1 ] && [ "$(val WantedBy)" = multi-user.target ] && echo 1 || echo 0)"
check "StartLimitIntervalSec=0 (rapid manual restarts must never leave the unit in start-limit-hit)" "$([ "$(val StartLimitIntervalSec)" = 0 ] && echo 1 || echo 0)"
check "Restart=no (bounded, never retried)" "$([ "$(val Restart)" = no ] && echo 1 || echo 0)"

# Bounded: the helper's own budgets must end it before systemd's backstop.
budget=$(grep -E '^BUDGET_SECONDS=' "$HELPER" | cut -d= -f2)
lockwait=$(grep -E '^LOCK_WAIT_SECONDS=' "$HELPER" | cut -d= -f2)
tmo=$(val TimeoutStartSec)
check "TimeoutStartSec is a small number of seconds and exceeds the helper's own lock wait + validation budget ($lockwait + $budget)" "$([ -n "$tmo" ] && [ "$tmo" -le 30 ] && [ "$tmo" -gt $(( budget + lockwait )) ] && echo 1 || echo 0)"

# --- The unit never touches SSH policy --------------------------------------
check "unit and helper never reference sshd_config / StrictModes / host keys / login policy" "$( { noncomment "$UNIT"; noncomment "$HELPER"; } | grep -q -i -E 'sshd_config|StrictModes|ssh_host_|AuthorizedKeysFile|AuthorizedKeysCommand|PermitRootLogin|PasswordAuthentication|systemctl (restart|reload|stop|start) ssh' && echo 0 || echo 1)"

# --- Makefile packaging -----------------------------------------------------
check "Makefile packages the unit into lib/systemd/system with mode 644" "$(grep -q 'cp debian/stratux_ssh_authorized_keys.service $(DEBPKG_BASE)/lib/systemd/system' Makefile && grep -q 'chmod 644 $(DEBPKG_BASE)/lib/systemd/system/stratux_ssh_authorized_keys.service' Makefile && echo 1 || echo 0)"

# --- postinst / prerm structure --------------------------------------------
enable_line=$(grep -n "^\s*systemctl enable $NAME\s*$" "$POSTINST" | head -1 | cut -d: -f1)
start_line=$(grep -n "^\s*systemctl start $NAME\s*$" "$POSTINST" | head -1 | cut -d: -f1)
ota_line=$(grep -n 'STRATUX_OTA_INSTALL:-' "$POSTINST" | head -1 | cut -d: -f1)
check "postinst enables the unit BEFORE the STRATUX_OTA_INSTALL early-exit (an OTA-delivered install still enables it)" "$([ -n "$enable_line" ] && [ -n "$ota_line" ] && [ "$enable_line" -lt "$ota_line" ] && echo 1 || echo 0)"
check "postinst starts it only AFTER the early-exit (never during an OTA install)" "$([ -n "$start_line" ] && [ "$start_line" -gt "$ota_line" ] && echo 1 || echo 0)"
check "postinst enables it inside the same Raspberry Pi/aarch64 guard as the other units" "$(awk '/if +\[ "\$arch" == "aarch64" \]; then/{a=1} a&&/systemctl enable stratux_ssh_authorized_keys/{f=1} /^fi$/{a=0} END{print f?1:0}' "$POSTINST")"
check "postinst makes the installed helper and unit root-owned (helper runs as root and decides who may log in)" "$(grep -q 'chown root:root /opt/stratux/bin/stratux-ssh-authorized-keys.sh /lib/systemd/system/stratux_ssh_authorized_keys.service' "$POSTINST" && echo 1 || echo 0)"
check "postinst never uses enable --now for it" "$(grep -q -E "enable +(--now|-\-now).*$NAME" "$POSTINST" && echo 0 || echo 1)"
check "neither script ever touches /var/lib/stratux-data (the administrator's keys survive upgrade and removal)" "$(cat "$POSTINST" "$PRERM" | grep -v '^\s*#' | grep -q 'stratux-data' && echo 0 || echo 1)"
check "neither script touches any ~/.ssh path or deletes/moves/copies an authorized_keys file" "$(cat "$POSTINST" "$PRERM" | grep -v '^\s*#' | grep -q -E '\.ssh|(^|[[:space:]])(rm|truncate|mv|cp|tee|sed)[[:space:]].*authorized' && echo 0 || echo 1)"
dash -n "$POSTINST" && dash -n "$PRERM" && echo "PASS: maintainer scripts are valid dash syntax" || { echo "FAIL: maintainer script syntax"; fail=1; }

# --- Execute the real maintainer scripts against stubs ----------------------
run_script() { # $1=script $2=logfile $3=extra env (may be empty) $4...=script args
	local script="$1" log="$2" extra="$3" sb; shift 3
	sb=$(mktemp -d); mkdir -p "$sb/bin" "$sb/boot"; : >"$sb/boot/config.txt"
	printf '#!/bin/sh\necho "systemctl $*" >> "%s"\nexit 0\n' "$log" >"$sb/bin/systemctl"
	printf '#!/bin/sh\n[ "$1" = "-m" ] && echo aarch64 || echo Linux\n' >"$sb/bin/uname"
	printf '#!/bin/sh\necho "chown $*" >> "%s"\nexit 0\n' "$log" >"$sb/bin/chown"
	printf '#!/bin/sh\necho "chmod $*" >> "%s"\nexit 0\n' "$log" >"$sb/bin/chmod"
	chmod +x "$sb/bin/"*
	sed "s#/boot/firmware/config.txt#$sb/boot/config.txt#g" "$script" >"$sb/script.sh"
	: >"$log"
	env $extra PATH="$sb/bin:$PATH" bash "$sb/script.sh" "$@" >/dev/null 2>&1
	rm -rf "$sb"
}
L=$(mktemp -d); trap 'rm -rf "$L"' EXIT

run_script "$POSTINST" "$L/post_ota.log" "STRATUX_OTA_INSTALL=1" configure
check "postinst (OTA install): enables it, chowns the helper to root, and starts NOTHING" "$(grep -qx "systemctl enable $NAME" "$L/post_ota.log" && grep -q 'chown root:root /opt/stratux/bin/stratux-ssh-authorized-keys.sh' "$L/post_ota.log" && ! grep -Eq 'systemctl (start|restart)' "$L/post_ota.log" && echo 1 || echo 0)"
run_script "$POSTINST" "$L/post_norm.log" "STRATUX_UNUSED=1" configure
check "postinst (normal install): enables it and starts it exactly once, after enabling" "$([ "$(grep -c "systemctl start $NAME" "$L/post_norm.log")" = 1 ] && [ "$(grep -n "enable $NAME" "$L/post_norm.log" | cut -d: -f1)" -lt "$(grep -n "start $NAME" "$L/post_norm.log" | cut -d: -f1)" ] && echo 1 || echo 0)"
check "postinst (normal install): the ownership fix runs before the unit is enabled" "$([ "$(grep -n 'chown root:root' "$L/post_norm.log" | head -1 | cut -d: -f1)" -lt "$(grep -n "enable $NAME" "$L/post_norm.log" | cut -d: -f1)" ] && echo 1 || echo 0)"
run_script "$PRERM" "$L/prerm_remove.log" "STRATUX_UNUSED=1" remove
check "prerm remove: disables the unit" "$(grep -qx "systemctl disable $NAME" "$L/prerm_remove.log" && echo 1 || echo 0)"
run_script "$PRERM" "$L/prerm_upgrade.log" "STRATUX_UNUSED=1" upgrade 2.0.0
check "prerm upgrade: does NOT disable it (postinst re-enables, and the enablement must survive)" "$(grep -q "disable $NAME" "$L/prerm_upgrade.log" && echo 0 || echo 1)"
run_script "$PRERM" "$L/prerm_ota.log" "STRATUX_OTA_INSTALL=1" upgrade 2.0.0
check "prerm (OTA install): touches no units" "$([ ! -s "$L/prerm_ota.log" ] && echo 1 || echo 0)"
run_script "$PRERM" "$L/prerm_noarg.log" "STRATUX_UNUSED=1"
check "prerm with no argument (as older tooling may call it): does not disable, does not fail" "$(grep -q "disable $NAME" "$L/prerm_noarg.log" && echo 0 || echo 1)"

# --- The real systemd parser ------------------------------------------------
if command -v systemd-analyze >/dev/null 2>&1; then
	tmp=$(mktemp -d)
	mkdir -p "$tmp/etc/systemd/system" "$tmp/opt/stratux/bin" "$tmp/usr/lib/systemd/system"
	cp -a /usr/lib/systemd/system/. "$tmp/usr/lib/systemd/system/" 2>/dev/null
	# A stand-in for Debian's ssh.service, ordered like the real one (After=network.target ...).
	printf '[Unit]\nDescription=stub ssh\nAfter=network.target auditd.service\n[Service]\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n' >"$tmp/etc/systemd/system/ssh.service"
	mkdir -p "$tmp/bin" "$tmp/usr/bin"; printf '#!/bin/sh\nexit 0\n' >"$tmp/bin/true"; chmod +x "$tmp/bin/true"; cp "$tmp/bin/true" "$tmp/usr/bin/true"
	cp "$UNIT" "$tmp/etc/systemd/system/"
	cp "$HELPER" "$tmp/opt/stratux/bin/stratux-ssh-authorized-keys.sh"; chmod +x "$tmp/opt/stratux/bin/stratux-ssh-authorized-keys.sh"
	out=$(systemd-analyze verify --root="$tmp" stratux_ssh_authorized_keys.service ssh.service 2>&1); rc=$?
	check "systemd-analyze verify accepts the unit together with ssh.service (no unknown directives, ordering resolves, no cycle)" "$([ -z "$out" ] && [ $rc = 0 ] && echo 1 || echo 0)"
	[ -n "$out" ] && printf '%s\n' "$out"
	rm -rf "$tmp"
else
	echo "SKIP: systemd-analyze not available; real-systemd unit parse not run"
fi

exit $fail
