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

# Confirms the build VM came back from the reboot in FIPS mode before the rest
# of the build runs. FIPS_ENABLED must be "true", otherwise this is a no-op.
set -o errexit -o nounset -o pipefail

if [[ "${FIPS_ENABLED:-false}" != "true" ]]; then
  exit 0
fi

fail() { echo "FIPS verification failed: $*" >&2; exit 1; }

[[ "$(cat /proc/sys/crypto/fips_enabled)" == "1" ]] || fail "kernel is not in FIPS mode (/proc/sys/crypto/fips_enabled != 1)"
grep -qw 'fips=1' /proc/cmdline || fail "fips=1 missing from the kernel command line"
[[ "$(update-crypto-policies --show)" == FIPS* ]] || fail "system crypto policy is $(update-crypto-policies --show), not FIPS"
fips-mode-setup --check
echo "Kernel $(uname -r) running in FIPS mode."
