#!/bin/bash
# ============================================================
# LogDiag OS Inspection Script - Linux
# Applies to : RHEL / CentOS / Rocky / AlmaLinux / Ubuntu / Debian /
#              SLES / openEuler / Anolis / Kylin / UOS and derivatives
# Usage      : run as root ->  bash linux-pm.sh
# Output     : /tmp/<hostname>.<serial>.pm-<timestamp>.tar.gz
# Note       : read-only collection, no system configuration is changed.
# ============================================================

HOSTNAME_STR=$(hostname 2>/dev/null || echo unknown)
SERIAL=$(dmidecode -s system-serial-number 2>/dev/null | grep -v '^#' | head -1 | tr -d ' /')
[ -z "$SERIAL" ] && SERIAL=$(cat /sys/class/dmi/id/product_serial 2>/dev/null | tr -d ' /')
[ -z "$SERIAL" ] && SERIAL=NotAvailable
STAMP=$(date +%Y%m%d_%H%M%S)
DIRNAME="${HOSTNAME_STR}.${SERIAL}.pm-${STAMP}"
CFGDIR="/tmp/${DIRNAME}"
mkdir -p "$CFGDIR"

if [ "$(id -u)" -ne 0 ]; then
    echo "WARNING: not running as root, some hardware/log data will be incomplete." | tee "$CFGDIR/warning.txt"
fi

# run <section/outfile> <command line>
# Every command is captured with its command line as header; failures are
# recorded but never abort the whole collection.
run() {
    OUT="$CFGDIR/$1"; shift
    mkdir -p "$(dirname "$OUT")"
    {
        echo "### $*"
        sh -c "$*" 2>&1 || echo "[command failed or not available]"
        echo ""
    } >> "$OUT"
}

echo "Collecting system information ..."
date > "$CFGDIR/date.log"
run system/os-release.log      "cat /etc/os-release"
run system/uname.log           "uname -a"
run system/hostnamectl.log     "hostnamectl"
run system/uptime.log          "uptime"
run system/last-reboot.log     "last -x reboot | head -20"
run system/timedatectl.log     "timedatectl"
run system/sysctl.log          "sysctl -a"
run system/kernel-cmdline.log  "cat /proc/cmdline"
run system/selinux.log         "getenforce; sestatus"
run system/crontab.log         "crontab -l; ls -l /etc/cron.d/"

echo "Collecting hardware information ..."
run hardware/dmidecode.log     "dmidecode"
run hardware/model.txt         "dmidecode -s system-product-name"
run hardware/serial.txt        "dmidecode -s system-serial-number"
run hardware/bios.txt          "dmidecode -s bios-version"
run hardware/lscpu.log         "lscpu"
run hardware/cpuinfo.log       "cat /proc/cpuinfo"
run hardware/meminfo.log       "cat /proc/meminfo"
run hardware/dmidecode-mem.log "dmidecode -t memory"
run hardware/lspci.log         "lspci -vv"
run hardware/lsusb.log         "lsusb"
run hardware/lsscsi.log        "lsscsi"
run hardware/sensors.log       "sensors"
run hardware/ipmi-sel.log      "ipmitool sel elist"
run hardware/ipmi-sensor.log   "ipmitool sdr elist"
run hardware/ipmi-fru.log      "ipmitool fru print"

echo "Collecting storage information ..."
run storage/lsblk.log          "lsblk -o NAME,MODEL,SERIAL,SIZE,TYPE,FSTYPE,MOUNTPOINT,STATE"
run storage/df.log             "df -hT"
run storage/inode.log          "df -i"
run storage/mount.log          "mount"
run storage/fstab.log          "cat /etc/fstab"
run storage/fdisk.log          "fdisk -l"
run storage/pvs.log            "pvs; pvdisplay"
run storage/vgs.log            "vgs; vgdisplay -v"
run storage/lvs.log            "lvs; lvdisplay"
run storage/mdstat.log         "cat /proc/mdstat; mdadm --detail --scan"
run storage/multipath.log      "multipath -ll"
run storage/iscsi.log          "iscsiadm -m session -P 3"
# SMART overview for every physical disk (read-only)
for DISK in $(lsblk -dn -o NAME,TYPE 2>/dev/null | awk '$2=="disk"{print $1}'); do
    run "storage/smart.${DISK}.log" "smartctl -H -i -A /dev/${DISK}"
done

echo "Collecting network information ..."
run network/ip-addr.log        "ip addr"
run network/ip-route.log       "ip route; ip -6 route"
run network/ip-link.log        "ip -s link"
run network/resolv.log         "cat /etc/resolv.conf"
run network/hosts.log          "cat /etc/hosts"
run network/listen.log         "ss -lntup"
run network/bonding.log        "cat /proc/net/bonding/* 2>/dev/null"
run network/firewall.log       "firewall-cmd --list-all; iptables -S; nft list ruleset"
for NIC in $(ls /sys/class/net/ 2>/dev/null | grep -v '^lo$'); do
    run "network/ethtool.${NIC}.log" "ethtool ${NIC}; ethtool -i ${NIC}; ethtool -S ${NIC} | head -80"
done

echo "Collecting software and service information ..."
run software/packages.log      "rpm -qa --last 2>/dev/null | head -500; dpkg -l 2>/dev/null"
run software/services.log      "systemctl list-units --type=service --state=running"
run software/failed-units.log  "systemctl --failed"
run software/enabled.log       "systemctl list-unit-files --state=enabled"

echo "Collecting error logs ..."
run logs/dmesg-err.log         "dmesg -T --level=emerg,alert,crit,err | tail -400"
run logs/journal-err.log       "journalctl -p err -b --no-pager | tail -600"
run logs/journal-prev-boot.log "journalctl -p err -b -1 --no-pager | tail -300"
run logs/messages-tail.log     "tail -500 /var/log/messages 2>/dev/null; tail -500 /var/log/syslog 2>/dev/null"
run logs/mcelog.log            "tail -200 /var/log/mcelog 2>/dev/null; edac-util -v"
run logs/oom.log               "dmesg -T | grep -i -E 'out of memory|oom-killer' | tail -50"

echo "Collecting performance snapshot (about 30 seconds) ..."
run performance/vmstat.log     "vmstat 3 10"
run performance/iostat.log     "iostat -x 3 5"
run performance/mpstat.log     "mpstat -P ALL 3 3"
run performance/sar-today.log  "sar 2>/dev/null | tail -60"
run performance/free.log       "free -m"
run performance/top10cpu.log   "ps aux --sort=-%cpu | head -11"
run performance/top10mem.log   "ps aux --sort=-%mem | head -11"
run performance/ps-count.log   "ps -ef | wc -l; ps -ef | grep -c defunct"
run performance/swap.log       "swapon -s; cat /proc/swaps"

echo "Compressing inspection data ..."
cd /tmp || exit 1
tar -czf "${DIRNAME}.tar.gz" "${DIRNAME}" && rm -rf "${CFGDIR}"
echo "Inspection data saved to /tmp/${DIRNAME}.tar.gz"
exit 0
