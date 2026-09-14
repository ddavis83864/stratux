#!/bin/bash -e

# install packages as part of the run portion of the script
# as they are necessary for the build.
#
# xx-packages will install to the PI so its not appropriate for build required packages

apt update

on_chroot << EOF
    apt install --yes dnsmasq ifplugd iptables

    # try to reduce writing to SD card as much as possible, so they don't get
    # bricked when yanking the power cable
    # Disable swap...
    systemctl disable dphys-swapfile
    apt purge -y dphys-swapfile

    systemctl disable dnsmasq # we start it manually on respective interfaces
    systemctl disable triggerhappy
    systemctl disable wpa_supplicant
    systemctl disable systemd-timesyncd # We sync time with GPS. Make sure there is no conflict if we have internet connection
    systemctl disable resize2fs_once

    systemctl disable apt-daily.timer
    systemctl disable apt-daily-upgrade.timer
    systemctl disable man-db.timer

    # Run DHCP on eth0 when cable is plugged in
    sed -i -e 's/INTERFACES=""/INTERFACES="eth0"/g' /etc/default/ifplugd

    # Do NOT generate SSH host keys at image-build time: a key generated
    # here is baked into this exact image file and is therefore shared,
    # byte-for-byte, by every device ever flashed from it - a real,
    # confirmed defect (see docs/known-limitations.md and
    # docs/releases/v2.0.0-rc2.md for the evidence). Unique keys are
    # instead generated once per device on that device's own first boot,
    # by stratux-ssh-hostkeys.service (enabled below).
    #
    # The stock Raspberry Pi OS regenerate_ssh_host_keys.service is not a
    # substitute here and stays disabled: it writes host keys and its own
    # self-disable through the plain /etc path, which under this image's
    # protected read-only overlay (init-overlay/overlayctl - see
    # docs/ota.md) lands only in the volatile tmpfs upper layer and is
    # discarded every reboot, so it would silently regenerate a brand-new
    # key set on every single boot rather than once per device.
    systemctl disable regenerate_ssh_host_keys
    # stratux-ssh-hostkeys.service is enabled further below, once its unit
    # file and script have actually been installed into the image (systemctl
    # enable needs the unit file to exist first).

    # rng-tools feeds this hardware's own RNG output into the kernel's
    # entropy pool from early boot. Physical validation of a genuinely
    # fresh clean-install boot found ssh-keygen -A blocking for many
    # minutes (dashboard and every other service fully up the whole time -
    # only key generation, which needs real random bytes, was stuck).
    # This project's base image does not install it by default (an
    # earlier attempt to just "systemctl enable rng-tools" without
    # installing it first failed the build outright - "unit rng-tools.
    # service does not exist" - caught by CI, not assumed). The exact
    # unit name a given Debian release installs it under has changed
    # across versions (rng-tools / rng-tools5 / rng-tools-debian), so
    # this discovers whatever unit the installed package actually
    # provides rather than hardcoding a name that could silently stop
    # matching on a future base-image bump.
    apt install --yes rng-tools5
    RNGUNIT="\$(systemctl list-unit-files 'rng*' --no-legend | awk '{print \$1}' | head -1)"
    if [ -n "\$RNGUNIT" ]; then
        systemctl enable "\$RNGUNIT"
    else
        echo "ERROR: rng-tools5 installed but no rng*.service unit found to enable" >&2
        exit 1
    fi
    # This is usually done by the console-setup service that takes quite long of first boot..
    /lib/console-setup/console-setup.sh

    # remove network-manager as Stratux depends on using ifupdown network interface
    apt remove -y network-manager
EOF

# install esptool
on_chroot << EOF
    apt install -y python3-pip
    pip install --break-system-packages esptool
EOF

# install bluez 5.79 version shipping with current RPiOS (5.66) is buggy in peripheral mode..
BLUEZ_DEB="bluez_5.79-1_arm64.deb"
on_chroot << EOF
    cd /tmp
    wget https://github.com/stratux/bluez/releases/download/v1.0/${BLUEZ_DEB}
    dpkg -i ${BLUEZ_DEB}
    rm ${BLUEZ_DEB}
EOF

LIBRTLSDR_DEB="librtlsdr0_2.0.2-2_arm64.deb"
LIBRTLSDR_DEV_DEB="librtlsdr-dev_2.0.2-2_arm64.deb"
RTLSDR_DEB="rtl-sdr_2.0.2-2_arm64.deb"
on_chroot << EOF
    echo "Installing librtlsdr in chroot"
    cd /tmp
    apt install -y libusb-1.0-0 libusb-1.0-0-dev
    wget https://github.com/stratux/rtlsdr/releases/download/v1.0/${LIBRTLSDR_DEB}
    wget https://github.com/stratux/rtlsdr/releases/download/v1.0/${LIBRTLSDR_DEV_DEB}
    dpkg -i ${LIBRTLSDR_DEB}
    dpkg -i ${LIBRTLSDR_DEV_DEB}
    rm ${LIBRTLSDR_DEB}
    rm ${LIBRTLSDR_DEV_DEB}

    echo "Installing rtlsdr"
    cd /tmp
    wget https://github.com/stratux/rtlsdr/releases/download/v1.0/${RTLSDR_DEB}
    dpkg -i ${RTLSDR_DEB}
    rm ${RTLSDR_DEB}

    echo "Building and installing kalibrate-rtl"

    apt install --yes build-essential autoconf libtool libfftw3-dev git

    # kalibrate-rtl
    cd /tmp
    git clone https://github.com/steve-m/kalibrate-rtl
    cd kalibrate-rtl
    ./bootstrap
    ./configure
    make -j8
    make install
    cd ../
    rm -rf kalibrate-rtl

    # remove the dev package of rtlsdr
    dpkg -r librtlsdr-dev

    # remove now unused libusb-1.0-0-dev
    apt remove -y libusb-1.0-0-dev
EOF

# Prepare wiringpi for ogn trx via GPIO
WIRINGPI_FILENAME="wiringpi_3.14_arm64.deb"
wget https://github.com/WiringPi/WiringPi/releases/download/3.14/${WIRINGPI_FILENAME}

# copy dpkg into /tmp directory so its available in the chroot
WIRINGPI_FILENAME_TMP=${ROOTFS_DIR}/tmp/${WIRINGPI_FILENAME}
(cp ${WIRINGPI_FILENAME} ${WIRINGPI_FILENAME_TMP})

# install the package
on_chroot << EOF
    dpkg -i /tmp/${WIRINGPI_FILENAME}
EOF

# remove the dpkg file from tmp
rm ${WIRINGPI_FILENAME_TMP}


install files/bashrc.txt ${ROOTFS_DIR}/root/.bashrc

install -m 644 files/motd "${ROOTFS_DIR}/etc/motd"


# network default config. TODO: can't we just implement gen_gdl90 -write_network_settings or something to generate them from template?
install files/stratux-dnsmasq.conf ${ROOTFS_DIR}/etc/dnsmasq.d/stratux-dnsmasq.conf

install files/wpa_supplicant_ap.conf ${ROOTFS_DIR}/etc/wpa_supplicant/wpa_supplicant_ap.conf
install files/interfaces ${ROOTFS_DIR}/etc/network/interfaces

# sshd config
install files/sshd_config ${ROOTFS_DIR}/etc/ssh/sshd_config

# debug aliases
install files/stxAliases.txt ${ROOTFS_DIR}/root/.stxAliases

# rtl-sdr setup
install files/rtl-sdr-blacklist.conf ${ROOTFS_DIR}/etc/modprobe.d/

# system tweaks
install files/modules.txt ${ROOTFS_DIR}/etc/modules

# boot settings
install files/config.txt ${ROOTFS_DIR}/boot/firmware/

# rootfs overlay stuff
install files/overlayctl files/init-overlay ${ROOTFS_DIR}/sbin/

on_chroot << EOF
    overlayctl install
    # init-overlay replaces raspis initial partition size growing.. Make sure we call that manually (see init-overlay script)
    touch /var/grow_root_part
    mkdir -p /overlay/robase # prepare so we can bind-mount root even if overlay is disabled
EOF

# unique per-device SSH host keys, generated on first boot (see the script's
# own comments and 01-run.sh's earlier ssh-keygen/regenerate_ssh_host_keys
# section for the full rationale) - must be installed after overlayctl
# above, since the script itself calls overlayctl, and enabled only after
# the unit file below actually exists in the image (systemctl enable reads
# the unit file's own [Install] section).
install files/stratux-generate-ssh-hostkeys ${ROOTFS_DIR}/usr/sbin/
install -m 644 files/stratux-ssh-hostkeys.service ${ROOTFS_DIR}/etc/systemd/system/

on_chroot << EOF
    systemctl enable stratux-ssh-hostkeys
EOF

# So we can import network settings if needed
touch ${ROOTFS_DIR}/boot/firmware/.stratux-first-boot

# startup scripts
install files/rc.local ${ROOTFS_DIR}/etc/rc.local

# Optionally mount /dev/sda1 as /var/log - for logging to USB stick
echo -e "\n/dev/sda1             /var/log        auto    defaults,nofail,noatime,x-systemd.device-timeout=1ms  0       2" >> ${ROOTFS_DIR}/etc/fstab

# disable serial console, disable rfkill state restore, enable wifi on boot
sed -i ${ROOTFS_DIR}/boot/firmware/cmdline.txt -e "s/console=serial0,[0-9]\+ /systemd.restore_state=0 rfkill.default_state=1 /"
sed -i 's/quiet//g' ${ROOTFS_DIR}/boot/firmware/cmdline.txt

# Set the keyboard layout to US.
sed -i ${ROOTFS_DIR}/etc/default/keyboard -e "/^XKBLAYOUT/s/\".*\"/\"us\"/"

# Set hostname
echo "stratux" > ${ROOTFS_DIR}/etc/hostname
sed -i ${ROOTFS_DIR}/etc/hosts -e "s/raspberrypi/stratux/g"
