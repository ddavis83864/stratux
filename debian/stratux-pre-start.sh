#!/bin/bash

#echo powersave >/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor

#Logging Function
SCRIPT=`basename ${BASH_SOURCE[0]}`
STX_LOG="/var/log/stratux.log"
function wLog () {
	echo "$(date +"%Y/%m/%d %H:%M:%S")  - $SCRIPT - $1" >> ${STX_LOG}
}
wLog "Running Stratux Updater Script."

TEMP_DIRECTORY="/boot/firmware/StratuxUpdates"

######################
# script based update
SCRIPT_MASK="update*stratux*v*.sh"
TEMP_SCRIPT_LOCATION="$TEMP_DIRECTORY/$SCRIPT_MASK"
SCRIPT_UPDATE_LOCATION="/root/$SCRIPT_MASK"

# Package-based (.deb) updates are handled below by the deterministic OTA
# state machine (OTA_DIR et al.), staged under the persistent data
# partition rather than this legacy TEMP_DIRECTORY - see that section for
# details.

# Detect whether the overlay filesystem is currently active.
# /overlay/robase is always present (used by overlayctl for management), so we
# check the root mount type instead of directory existence.
overlay_is_active() {
	[ "$(findmnt -n -o FSTYPE /)" = "overlay" ]
}

###############
# Stage 1 (overlay active): script in SD card download location.
# Copy it directly to the lower ext4 layer so it survives the reboot.
if [ -e ${TEMP_SCRIPT_LOCATION} ]; then
	TEMP_SCRIPT_FILE=`ls -1t ${TEMP_SCRIPT_LOCATION} | head -1`
	wLog "Found update script $TEMP_SCRIPT_FILE"
	if overlay_is_active; then
		wLog "Overlay active — staging script to ext4 lower layer and disabling overlay..."
		if /sbin/overlayctl unlock && cp "${TEMP_SCRIPT_FILE}" /overlay/robase/root/; then
			chmod a+x /overlay/robase/root/$(basename "${TEMP_SCRIPT_FILE}")
			rm -f "${TEMP_SCRIPT_FILE}"
			/sbin/overlayctl disable
			wLog "Script staged. Rebooting to apply on bare ext4..."
			sync
			reboot
			exit 0
		else
			wLog "ERROR: Failed to stage script to ext4 lower layer. Update aborted."
			/sbin/overlayctl lock
		fi
	else
		# Overlay already inactive — copy to /root/ for next section to pick up
		cp "${TEMP_SCRIPT_FILE}" /root/
		chmod a+x /root/$(basename "${TEMP_SCRIPT_FILE}")
		rm -f "${TEMP_SCRIPT_FILE}"
	fi
fi

# Stage 2 (overlay inactive): execute the update script from /root/
if [ -e ${SCRIPT_UPDATE_LOCATION} ]; then
	UPDATE_SCRIPT_FILE=`ls -1t ${SCRIPT_UPDATE_LOCATION} | head -1`
	if [ -n "${UPDATE_SCRIPT_FILE}" ]; then
		wLog "Executing update script ${UPDATE_SCRIPT_FILE}..."
		# Move to /tmp and re-enable overlay before running, in case the script
		# triggers a service restart that kills this ExecStartPre process.
		UPDATE_TEMP_SCRIPT="/tmp/$(basename "${UPDATE_SCRIPT_FILE}")"
		mv "${UPDATE_SCRIPT_FILE}" "${UPDATE_TEMP_SCRIPT}"
		/sbin/overlayctl enable
		if bash "${UPDATE_TEMP_SCRIPT}"; then
			wLog "Update script completed successfully."
		else
			wLog "ERROR: Update script ${UPDATE_SCRIPT_FILE} failed."
		fi
		rm -f "${UPDATE_TEMP_SCRIPT}"
		wLog "Finished. Rebooting..."
		sync
		reboot
		exit 0
	fi
fi

##############
# Deterministic, resumable package-update (.deb) state machine.
#
# State lives in a single JSON file on the persistent data partition (not
# /boot/firmware, not the overlay), written by both this script and the Go
# daemon (main/ota.go) - see the `ota` package for the canonical stage/
# action definitions this shell logic re-derives.
#
# The one persistent marker location - proven on real hardware by
# device-number identity, not assumed - is /overlay/robase/overlay/disable
# while the overlay is active: it shares its device number with the real
# mounted ext4 lower root. A lookalike path,
# /overlay/pivot/overlay/disable, is a tmpfs shadow left mounted there by
# init-overlay's own mount choreography (the original top-level /overlay
# tmpfs mount is never explicitly relocated when its named children are
# moved into the pivoted root) and does NOT survive a reboot. Writing the
# marker therefore always follows the same narrow sequence: remount
# /overlay/robase read-write, write, sync, remount /overlay/robase
# read-only. See docs/ota.md for the full evidence.
OTA_DIR="/var/lib/stratux-data/updates"
OTA_STATE="${OTA_DIR}/state.json"
OTA_BACKUP_DIR="${OTA_DIR}/backup"

ota_log() {
	wLog "OTA: $1"
}

ota_save_stage() {
	# $1: new Stage value. $2 (optional): LastError to record.
	if [ -n "$2" ]; then
		jq --arg s "$1" --arg e "$2" '.Stage=$s | .LastError=$e' "${OTA_STATE}" > "${OTA_STATE}.tmp" && mv "${OTA_STATE}.tmp" "${OTA_STATE}"
	else
		jq --arg s "$1" '.Stage=$s' "${OTA_STATE}" > "${OTA_STATE}.tmp" && mv "${OTA_STATE}.tmp" "${OTA_STATE}"
	fi
	sync
}

# ota_request_overlay_enable removes the persistent disable marker so the
# *next* boot returns to the protected overlay. Safe to call whether the
# overlay is currently active (narrow remount-rw/write/sync/relock-ro) or
# already inactive (bare ext4 root is directly writable - no remount
# dance needed).
ota_request_overlay_enable() {
	if overlay_is_active; then
		/sbin/overlayctl unlock || return 1
		rm -f /overlay/robase/overlay/disable
		sync
		/sbin/overlayctl lock
	else
		rm -f /overlay/disable
		sync
	fi
}

if [ -f "${OTA_STATE}" ]; then
	OTA_STAGE="$(jq -r .Stage "${OTA_STATE}" 2>/dev/null)"
	OTA_PACKAGE="$(jq -r .PackagePath "${OTA_STATE}" 2>/dev/null)"
	OTA_SHA256="$(jq -r .ExpectedSHA256 "${OTA_STATE}" 2>/dev/null)"
	OTA_ATTEMPTS="$(jq -r '.Attempts // 0' "${OTA_STATE}" 2>/dev/null)"
	ota_log "state found: stage=${OTA_STAGE} package=${OTA_PACKAGE} attempts=${OTA_ATTEMPTS}"

	# --- Install stage: only ever runs once bare ext4 is confirmed. ---
	# Must trigger on both the first attempt (Stage == disable_requested)
	# and any resumed attempt after a retry reboot or an interrupted
	# install / power loss (Stage == installing). A guard that only
	# matched disable_requested would wedge the device forever after the
	# first failed dpkg -i: Stage advances to "installing" before the
	# retry reboot, so this block would never run again - Attempts frozen,
	# overlay never re-enabled. Caught by dry-run test before this fix
	# landed; see docs/ota.md.
	if { [ "${OTA_STAGE}" = "disable_requested" ] || [ "${OTA_STAGE}" = "installing" ]; } && ! overlay_is_active; then
		RESUMING=false
		[ "${OTA_STAGE}" = "installing" ] && RESUMING=true

		if $RESUMING; then
			# A previous dpkg -i may have actually completed just before
			# a reboot or power loss cut this script off before it could
			# record "installed" - re-derive success from dpkg's own
			# status rather than blindly retrying or blindly trusting
			# the stage name.
			DPKG_STATUS="$(dpkg-query -W -f='${Status}' stratux 2>/dev/null)"
			DPKG_VERSION="$(dpkg-query -W -f='${Version}' stratux 2>/dev/null)"
			EXPECTED_VERSION="$(jq -r .ExpectedVersion "${OTA_STATE}" 2>/dev/null)"
			if [ "${DPKG_STATUS}" = "install ok installed" ] && [ "${DPKG_VERSION}" = "${EXPECTED_VERSION}" ]; then
				ota_log "resumed installing stage: dpkg already reports this version healthy - prior attempt actually succeeded"
				ota_save_stage "installed"
				ota_request_overlay_enable
				ota_log "re-enabled overlay for next boot; rebooting"
				sync
				reboot
				exit 0
			fi
			if [ "${OTA_ATTEMPTS}" -ge 3 ]; then
				ota_log "exhausted install attempts on resume; marking failed for rollback"
				ota_save_stage "failed" "dpkg -i did not succeed after ${OTA_ATTEMPTS} attempts"
				ota_request_overlay_enable
				sync
				reboot
				exit 0
			fi
		fi

		if [ ! -e "${OTA_PACKAGE}" ]; then
			ota_log "ERROR: staged package missing on bare root (${OTA_PACKAGE})"
			ota_save_stage "failed" "staged package missing on bare root"
			if $RESUMING; then
				ota_request_overlay_enable
				sync
				reboot
				exit 0
			fi
		else
			ACTUAL_SHA256="$(sha256sum "${OTA_PACKAGE}" | cut -d' ' -f1)"
			if [ "${ACTUAL_SHA256}" != "${OTA_SHA256}" ]; then
				ota_log "ERROR: hash mismatch on bare root (got ${ACTUAL_SHA256}, expected ${OTA_SHA256})"
				ota_save_stage "failed" "hash mismatch on bare root"
				if $RESUMING; then
					ota_request_overlay_enable
					sync
					reboot
					exit 0
				fi
			else
				# Bare ext4 root is directly writable - no remount dance
				# needed for the backup or for dpkg itself. On a resumed
				# attempt, reuse the backup already taken before the
				# first attempt rather than taking (and retaining) a new
				# one per retry.
				if $RESUMING; then
					BACKUP_FILE="$(jq -r '.BackupPath // empty' "${OTA_STATE}" 2>/dev/null)"
				else
					mkdir -p "${OTA_BACKUP_DIR}"
					BACKUP_FILE="${OTA_BACKUP_DIR}/pre-install-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"
					ota_log "backing up current install to ${BACKUP_FILE} before installing"
					tar czf "${BACKUP_FILE}" -C / opt/stratux lib/systemd/system/stratux.service lib/systemd/system/stratux_fancontrol.service etc/udev/rules.d/10-stratux.rules 2>>"${STX_LOG}"
				fi

				jq --arg b "${BACKUP_FILE}" '.Stage="installing" | .BackupPath=$b | .Attempts=((.Attempts // 0)+1)' "${OTA_STATE}" > "${OTA_STATE}.tmp" && mv "${OTA_STATE}.tmp" "${OTA_STATE}"
				sync

				ota_log "installing ${OTA_PACKAGE} (attempt $((OTA_ATTEMPTS+1)))"
				DPKG_OUTPUT="$(dpkg -i --force-depends "${OTA_PACKAGE}" 2>&1)"
				DPKG_RC=$?
				echo "${DPKG_OUTPUT}" >> "${STX_LOG}"

				if [ ${DPKG_RC} -eq 0 ]; then
					ota_log "dpkg install succeeded"
					ota_save_stage "installed"
					ota_request_overlay_enable
					ota_log "re-enabled overlay for next boot; rebooting"
					sync
					reboot
					exit 0
				else
					ota_log "ERROR: dpkg -i failed (rc=${DPKG_RC}, attempt $((OTA_ATTEMPTS+1)))"
					if [ "$((OTA_ATTEMPTS+1))" -ge 3 ]; then
						ota_log "exhausted install attempts; marking failed for rollback"
						ota_save_stage "failed" "dpkg -i failed after 3 attempts: ${DPKG_OUTPUT}"
						ota_request_overlay_enable
						sync
						reboot
						exit 0
					fi
					# Still have attempts left - stay on bare ext4 and
					# reboot to retry. Stage remains "installing", and
					# the top-level guard above now matches that stage
					# too, so the next boot re-enters this same block.
					sync
					reboot
					exit 0
				fi
			fi
		fi
	fi

	# --- Failure/rollback: restore the pre-install backup and always
	#     re-enable the overlay before returning to normal operation. ---
	if [ "${OTA_STAGE}" = "failed" ]; then
		if overlay_is_active; then
			ota_log "ERROR: failed state found while overlay is active - unexpected; requesting disable to run rollback on bare root"
			if /sbin/overlayctl unlock; then
				echo 1 > /overlay/robase/overlay/disable
				sync
				/sbin/overlayctl lock
				sync
				reboot
				exit 0
			fi
		else
			BACKUP_FILE="$(jq -r '.BackupPath // empty' "${OTA_STATE}" 2>/dev/null)"
			if [ -n "${BACKUP_FILE}" ] && [ -e "${BACKUP_FILE}" ]; then
				ota_log "rolling back using ${BACKUP_FILE}"
				tar xzf "${BACKUP_FILE}" -C / 2>>"${STX_LOG}"
				systemctl daemon-reload 2>>"${STX_LOG}"
				ota_log "rollback restore complete"
			else
				ota_log "ERROR: no usable backup found for rollback ($(jq -r '.LastError // empty' "${OTA_STATE}"))"
			fi
			ota_save_stage "rolled_back"
			ota_request_overlay_enable
			ota_log "re-enabled overlay for next boot; rebooting"
			sync
			reboot
			exit 0
		fi
	fi
fi


if [ -f /boot/firmware/.stratux-first-boot ]; then
	rm /boot/firmware/.stratux-first-boot
	if [ -f /boot/firmware/stratux.conf ]; then
		# In case of US build, a stratux.conf file will always be imported, only containing UAT/OGN options.
		# We don't want to force-reboot for that.. Only for network/overlay changes
		do_reboot=false

		# re-apply overlay
		if [ "$(jq -r .PersistentLogging /boot/firmware/stratux.conf)" = "true" ]; then
			/sbin/overlayctl disable
			do_reboot=true
			wLog "overlayctl disabled due to stratux.conf settings"
		fi

		# write network config
		if grep -q WiFi /boot/firmware/stratux.conf ; then
			/opt/stratux/bin/stratuxrun -write-network-config
			do_reboot=true
			wLog "re-wrote network configuration for first-boot config import. Rebooting... Bye"
		fi
		if $do_reboot; then
			sync
			reboot
			exit 0
		fi
	fi
fi

###############
# Standardize the appliance on UTC. Stored timestamps (health events, and
# future recording data) are UTC regardless of system timezone, but the
# system timezone itself has shipped as a stale build-time default
# (Europe/London) on some images. This runs on every boot, is a no-op
# once corrected, and uses the overlay unlock/lock pattern already used
# elsewhere in this script so the fix persists through the protected
# read-only root exactly like a real image rebuild would - an
# already-flashed card gets the correct, persistent timezone without
# needing to be reflashed.
CURRENT_TZ="$(cat /etc/timezone 2>/dev/null)"
if [ "$CURRENT_TZ" != "UTC" ]; then
	wLog "System timezone is '${CURRENT_TZ}', correcting to UTC..."
	if overlay_is_active; then
		if /sbin/overlayctl unlock; then
			echo "UTC" > /overlay/robase/etc/timezone
			ln -sf /usr/share/zoneinfo/UTC /overlay/robase/etc/localtime
			/sbin/overlayctl lock
			wLog "Timezone corrected to UTC (persisted through the overlay's lower layer)."
		else
			wLog "ERROR: could not unlock overlay to correct timezone; will retry next boot."
		fi
	else
		echo "UTC" > /etc/timezone
		ln -sf /usr/share/zoneinfo/UTC /etc/localtime
		wLog "Timezone corrected to UTC (overlay inactive, wrote directly)."
	fi
fi

wLog "Exited without updating anything..."
