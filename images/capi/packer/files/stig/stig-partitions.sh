#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# DISA STIG separate filesystems for /var, /var/log, /var/log/audit,
# /var/tmp, /home and /tmp (V-257843 to V-257848).
#
# Cloud images have a single XFS root partition, and XFS can't shrink. STIG
# builds therefore stop cloud-init growing the root partition (see
# local.user_data), and this script puts an LVM volume group in the rest of
# the build disk:
#
#   create  (before the FIPS reboot) adds a partition after the last one,
#           creates a logical volume per mount point, copies the current
#           data onto them and adds them to /etc/fstab; the reboot mounts them.
#   finish  (after the reboot) checks they are mounted and deletes the old
#           copies hidden under the mount points on the root filesystem
#           (they include cloud-init's copy of the build user-data).
#
# /var gets all the space the other volumes don't use, and on nodes
# grow-var-lv.service grows /var into the rest of a disk larger than the
# image. Mount options (nodev, noexec, nosuid) are left to the STIG
# remediation, so the provisioning steps before it run as before.
#
# Mount points the source image already has as separate filesystems (for
# example a Prism Central image installed with a STIG kickstart) are left
# alone; if there are none to create this script does nothing.
#
# Runs as root. No-op unless STIG_ENABLED is "true".
set -o errexit -o nounset -o pipefail

if [[ "${STIG_ENABLED:-false}" != "true" ]]; then
  exit 0
fi
PHASE=${1:?usage: stig-partitions.sh create|finish}

VG=vg_node
# Sizes of the fixed volumes; /var gets the rest, at least VAR_MIN_GIB.
# kubelet keeps container logs under /var/log/pods.
SIZES=${STIG_PARTITION_SIZES:-"var_log=8G var_log_audit=4G var_tmp=2G home=1G tmp=2G"}
VAR_MIN_GIB=${STIG_VAR_MIN_GIB:-10}
GOSS_DIR=/opt/image-builder

# Volume name -> mount point. Parents come before children.
LVS=(var var_log var_log_audit var_tmp home tmp)
declare -A MP=(
  [var]=/var [var_log]=/var/log [var_log_audit]=/var/log/audit
  [var_tmp]=/var/tmp [home]=/home [tmp]=/tmp
)

section() { printf '\n==> %s\n' "$*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

size_of() { # size_of <lv> -> e.g. 2G, from SIZES
  local kv
  for kv in ${SIZES}; do
    [[ ${kv%%=*} == "$1" ]] && { echo "${kv#*=}"; return; }
  done
  die "no size for $1 in STIG_PARTITION_SIZES (${SIZES})"
}

# True if another volume we create is mounted above this one, so its data is
# copied (and later cleaned up) along with the parent.
has_created_parent() {
  local lv=$1 other
  for other in "${CREATE[@]}"; do
    [[ $other != "$lv" && ${MP[$lv]} == "${MP[$other]}"/* ]] && return 0
  done
  return 1
}

install_grow_service() {
  # Nodes are cloned onto disks larger than the image. cloud-init can only
  # grow the root partition, which is no longer the last one, so grow the
  # volume group's partition and /var at boot instead. The script lives in
  # /usr/local/sbin so the STIG role adds it to the fapolicyd trust database.
  cat >/usr/local/sbin/grow-var-lv <<EOF
#!/usr/bin/env bash
# Grows /var (${VG}/var) into unused space at the end of its disk. Installed by
# image-builder's DISA STIG build; runs at every boot and does nothing when
# there is no new space.
set -o errexit -o nounset -o pipefail
pv=\$(pvs --noheadings -o pv_name --select vg_name=${VG} | head -1 | tr -d ' ')
[[ -n "\${pv}" ]] || exit 0
disk=/dev/\$(lsblk -no PKNAME "\${pv}" | head -1)
partnum=\$(cat "/sys/class/block/\$(basename "\$(readlink -f "\${pv}")")/partition")
# growpart: 0 grown, 1 nothing to do, anything else is an error.
rc=0; growpart "\${disk}" "\${partnum}" || rc=\$?
[[ \${rc} -le 1 ]] || exit "\${rc}"
pvresize "\${pv}"
if [[ \$(vgs --noheadings -o vg_free_count ${VG} | tr -d ' ') -gt 0 ]]; then
  lvextend --resizefs -l +100%FREE ${VG}/var
fi
EOF
  chmod 0755 /usr/local/sbin/grow-var-lv
  cat >/etc/systemd/system/grow-var-lv.service <<EOF
[Unit]
Description=Grow /var into unused disk space
After=local-fs.target
Before=containerd.service kubelet.service
ConditionPathExists=/dev/${VG}/var

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/grow-var-lv

[Install]
WantedBy=multi-user.target
EOF
  systemctl enable grow-var-lv.service
}

create() {
  CREATE=()
  local lv
  for lv in "${LVS[@]}"; do
    mountpoint -q "${MP[$lv]}" || CREATE+=("$lv")
  done
  if [[ ${#CREATE[@]} -eq 0 ]]; then
    echo "The source image already has separate filesystems for ${MP[*]}."
    return
  fi
  section "Creating separate filesystems for: $(for lv in "${CREATE[@]}"; do printf '%s ' "${MP[$lv]}"; done)"

  # Packages: LVM, and growpart for grow-var-lv on nodes.
  dnf -y install lvm2 cloud-utils-growpart

  # Disk holding the root filesystem (walk up through any device mapper).
  local dev disk
  dev=$(findmnt -no SOURCE /)
  while [[ $(lsblk -dno TYPE "${dev}") != disk ]]; do
    dev=$(lsblk -npdo PKNAME "${dev}" | head -1)
    [[ -n "${dev}" ]] || die "can't find the disk holding /"
  done
  disk=${dev}
  echo "Root disk: ${disk}"

  # The disk was enlarged from the image size when the VM was created; move
  # the backup GPT header to the new end so the free space can be used.
  if [[ $(blkid -po value -s PTTYPE "${disk}") == gpt ]]; then
    sfdisk --relocate gpt-bak-std "${disk}"
  fi

  local free_bytes need_gib=0 s
  free_bytes=$(sfdisk -F "${disk}" | sed -n '1s/.*, \([0-9]*\) bytes,.*/\1/p')
  for lv in "${CREATE[@]}"; do
    [[ $lv == var ]] && { need_gib=$((need_gib + VAR_MIN_GIB)); continue; }
    s=$(size_of "$lv")
    need_gib=$((need_gib + $(numfmt --from=iec "${s}") / 1073741824))
  done
  echo "Unpartitioned space: $((free_bytes / 1073741824)) GiB, needed: ${need_gib} GiB (+ ~1 GiB overhead)"
  if (( free_bytes / 1073741824 < need_gib + 1 )); then
    die "not enough free disk space for the STIG filesystems; increase the build's disk size (disk_size_gb)"
  fi

  # Partition after the last one, type Linux LVM.
  local before part
  before=$(lsblk -nlpo NAME "${disk}")
  echo ',,E6D6D379-F507-44C2-A23C-238F2A3DF928' | sfdisk --append --force --no-reread "${disk}"
  partx --add "${disk}" 2>/dev/null || partx --update "${disk}"
  udevadm settle
  part=$(comm -13 <(echo "${before}" | sort) <(lsblk -nlpo NAME "${disk}" | sort) | head -1)
  [[ -b "${part}" ]] || die "the new partition on ${disk} did not appear"
  echo "New partition: ${part}"

  # LVM's devices file pins PVs to this VM's disk IDs; every node is a clone
  # with different IDs, so it would ignore the PV and fail to mount /var.
  sed -i -E 's/^([[:space:]]*)#?[[:space:]]*use_devicesfile[[:space:]]*=.*/\1use_devicesfile = 0/' /etc/lvm/lvm.conf
  grep -qE '^[[:space:]]*use_devicesfile = 0' /etc/lvm/lvm.conf || die "could not disable the LVM devices file"
  rm -f /etc/lvm/devices/system.devices

  pvcreate -y "${part}"
  vgcreate -y "${VG}" "${part}"
  for lv in "${CREATE[@]}"; do
    [[ $lv == var ]] && continue
    lvcreate -y -n "$lv" -L "$(size_of "$lv")" "${VG}"
  done
  if [[ " ${CREATE[*]} " == *" var "* ]]; then
    lvcreate -y -n var -l 100%FREE "${VG}"
  fi
  for lv in "${CREATE[@]}"; do
    mkfs.xfs -q "/dev/${VG}/${lv}"
  done

  section "Copying data"
  local new=/mnt/stig-new
  mkdir -p "${new}"
  for lv in "${CREATE[@]}"; do
    mkdir -p "${new}${MP[$lv]}"
    mount "/dev/${VG}/${lv}" "${new}${MP[$lv]}"
  done
  for lv in "${CREATE[@]}"; do
    has_created_parent "$lv" && continue
    # -x: don't descend into other filesystems mounted below (e.g. rpc_pipefs).
    cp -ax "${MP[$lv]}/." "${new}${MP[$lv]}/"
  done
  # Each new filesystem's top directory takes the original's owner, mode and
  # SELinux label.
  for lv in "${CREATE[@]}"; do
    chown --reference="${MP[$lv]}" "${new}${MP[$lv]}"
    chmod --reference="${MP[$lv]}" "${new}${MP[$lv]}"
    if selinuxenabled 2>/dev/null; then
      chcon --reference="${MP[$lv]}" "${new}${MP[$lv]}"
    fi
  done
  for ((i = ${#CREATE[@]} - 1; i >= 0; i--)); do
    umount "${new}${MP[${CREATE[$i]}]}"
  done
  rm -rf "${new}"

  # /dev/mapper paths are the same on every clone.
  section "Adding the filesystems to /etc/fstab"
  sed -i "\|^/dev/mapper/${VG}-|d" /etc/fstab
  for lv in "${CREATE[@]}"; do
    printf '/dev/mapper/%s-%s %s xfs defaults 0 0\n' "${VG}" "${lv}" "${MP[$lv]}" | tee -a /etc/fstab
  done
  systemctl daemon-reload

  if [[ " ${CREATE[*]} " == *" var "* ]]; then
    install_grow_service
    # Generic cloud images (dracut hostonly=no) carry the dracut nfs module,
    # which mounts rpc_pipefs at /var/lib/nfs/rpc_pipefs from the initramfs,
    # before /var is mounted, leaving it hidden under the new /var. Nodes
    # don't boot from NFS, so leave the module out. The fips module stays.
    section "Rebuilding the initramfs without the dracut nfs module"
    echo 'omit_dracutmodules+=" nfs "' >/etc/dracut.conf.d/90-image-builder-stig.conf
    dracut -f --regenerate-all
  fi
  echo "The filesystems are mounted at the next boot."
}

finish() {
  local lv mp parent tmp rel lvs
  lvs=$(lvs --noheadings -o lv_name "${VG}" 2>/dev/null | tr -d ' ' || true)
  CREATE=()
  for lv in "${LVS[@]}"; do
    grep -qx "$lv" <<<"${lvs}" && CREATE+=("$lv")
  done
  if [[ ${#CREATE[@]} -eq 0 ]]; then
    echo "No STIG filesystems were created by this build."
  else
    section "Checking the STIG filesystems are mounted"
    for lv in "${CREATE[@]}"; do
      [[ $(findmnt -no SOURCE "${MP[$lv]}") == "/dev/mapper/${VG}-${lv}" ]] ||
        die "${MP[$lv]} is not mounted from ${VG}/${lv}"
    done
    for lv in "${CREATE[@]}"; do findmnt -no TARGET,SOURCE,SIZE "${MP[$lv]}"; done
    # Anything mounted below one of them before it was mounted (e.g. by the
    # initramfs) is hidden underneath it: its parent mount is outside it.
    local hidden
    hidden=$(awk -v mps="$(for lv in "${CREATE[@]}"; do printf '%s ' "${MP[$lv]}"; done)" '
      BEGIN { n = split(mps, m, " ") }
      { path[$1] = $5; parent[$1] = $2 }
      END {
        for (id in path) for (j = 1; j <= n; j++) {
          p = m[j]; q = path[parent[id]]
          if (index(path[id], p "/") == 1 && q != p && index(q, p "/") != 1) print path[id]
        }
      }' /proc/self/mountinfo | sort -u | tr '\n' ' ')
    [[ -z "${hidden}" ]] || die "mounted before the STIG filesystems and hidden under them: ${hidden}"

    section "Deleting the old copies under the mount points"
    for lv in "${CREATE[@]}"; do
      has_created_parent "$lv" && continue
      mp=${MP[$lv]}
      # The old data is on the filesystem holding the parent directory; a
      # non-recursive bind mount of it shows what the new mount hides.
      parent=$(findmnt -no TARGET --target "$(dirname "${mp}")")
      rel=${mp#"${parent%/}"}
      tmp=$(mktemp -d /run/stig-old.XXXXXX)
      mount --bind "${parent}" "${tmp}"
      if [[ -d "${tmp}${rel}" && $(stat -c %d "${tmp}${rel}") == $(stat -c %d "${tmp}") ]]; then
        find "${tmp}${rel}" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
        echo "cleaned ${mp} on ${parent}"
      fi
      umount "${tmp}"
      rmdir "${tmp}"
    done
  fi

  # goss runs after the STIG remediation makes /tmp noexec, so it is
  # downloaded here instead (see local.goss_download_path). Removed by
  # finalize-stig.sh.
  install -d -m 0755 -o "${BUILD_USER:?BUILD_USER must be set}" "${GOSS_DIR}"
}

case "${PHASE}" in
  create) create ;;
  finish) finish ;;
  *) die "unknown phase ${PHASE}" ;;
esac
