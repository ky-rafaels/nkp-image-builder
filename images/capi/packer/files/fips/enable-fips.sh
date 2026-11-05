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

# Switches an EL9 build VM to FIPS 140 mode: the kernel boots with fips=1
# (its crypto API self-tests and restricts itself to approved algorithms), the
# initramfs carries the dracut fips module, and the system-wide crypto policy
# is FIPS, so OpenSSL, GnuTLS, NSS, libgcrypt and OpenSSH run in their
# validated modes. Takes effect on the next boot; the build reboots right after
# this script so the rest of the build (and goss) runs in FIPS mode.
#
# Runs as root. FIPS_ENABLED must be "true", otherwise this is a no-op.
set -o errexit -o nounset -o pipefail

if [[ "${FIPS_ENABLED:-false}" != "true" ]]; then
  echo "FIPS not requested; skipping."
  exit 0
fi

. /etc/os-release
major="${VERSION_ID%%.*}"
case "${ID}:${major}" in
  rhel:9 | rocky:9 | almalinux:9 | centos:9) ;;
  *)
    echo "FIPS mode is only supported on EL9 (RHEL/Rocky/Alma 9) here; this is ${PRETTY_NAME}." >&2
    exit 1
    ;;
esac

if ! command -v fips-mode-setup >/dev/null 2>&1; then
  echo "Installing crypto-policies-scripts (provides fips-mode-setup)..."
  dnf -y install crypto-policies-scripts
fi

if fips-mode-setup --is-enabled; then
  echo "FIPS mode already enabled."
  exit 0
fi

# Sets the FIPS crypto policy, adds the dracut fips module and regenerates the
# initramfs, and adds fips=1 (and boot=UUID=… when /boot is separate) to every
# installed kernel's command line via grubby.
fips-mode-setup --enable

# Kernels installed later (e.g. by the build's `dnf update`) take their
# command line from /etc/kernel/cmdline when it exists, else from the running
# kernel's. Make sure fips=1 survives either way.
if [[ -f /etc/kernel/cmdline ]] && ! grep -qw 'fips=1' /etc/kernel/cmdline; then
  sed -i 's/$/ fips=1/' /etc/kernel/cmdline
fi

echo "FIPS mode configured; rebooting to apply."
