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

# Runs once the build VM is back from the GPU reboot (Packer retries it until
# SSH works), and fails early, before the main provisioning run, unless the
# attached NVIDIA GPU is on the PCI bus and nouveau has stayed off it.
# Runs as root. GPU_ENABLED must be "true", otherwise this is a no-op.
set -o errexit -o nounset -o pipefail

if [[ "${GPU_ENABLED:-false}" != "true" ]]; then
  exit 0
fi

gpus=()
for d in /sys/bus/pci/devices/*; do
  # NVIDIA display (0x0300) and 3D (0x0302) controllers, not their audio functions.
  [[ "$(cat "$d/vendor")" == 0x10de && "$(cat "$d/class")" == 0x03* ]] || continue
  gpus+=("${d##*/} (device $(cat "$d/device"))")
done

if [[ ${#gpus[@]} -eq 0 ]]; then
  echo "GPU check failed: no NVIDIA GPU on the build VM's PCI bus, although \"${GPU_NAME:-}\" was attached." >&2
  echo "Only NVIDIA GPUs are supported. GPUs with large BARs (A100, H100, ...) need boot_type=uefi." >&2
  exit 1
fi
if grep -qw '^nouveau' /proc/modules; then
  echo "GPU check failed: nouveau is loaded despite /etc/modprobe.d/blocklist-nouveau.conf." >&2
  exit 1
fi
echo "Kernel $(uname -r); NVIDIA GPU for \"${GPU_NAME:-}\": ${gpus[*]}"
