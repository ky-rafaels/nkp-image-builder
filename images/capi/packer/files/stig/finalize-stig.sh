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

# Last step of a DISA STIG build, after goss and just before shutdown:
#   1. applies the rules held back while provisioning (the build user's
#      NOPASSWD sudo rule, which goss needed),
#   2. checks the container runtime still works with fapolicyd enforcing,
#   3. rebuilds the AIDE baseline from the finished image,
#   4. scans the image and stores the results in the image as evidence,
#   5. gives the build user password sudo for Packer's shutdown_command.
# Runs as root. STIG_ENABLED must be "true", otherwise this is a no-op.
set -o errexit -o nounset -o pipefail

if [[ "${STIG_ENABLED:-false}" != "true" ]]; then
  exit 0
fi
: "${BUILD_USER:?BUILD_USER (the Packer SSH user) must be set}"

REPORT_DIR=/usr/share/image-builder/stig
# shellcheck source=/dev/null
. "${REPORT_DIR}/stig.env"

section() { printf '\n==> %s\n' "$*"; }

oscap_eval() {
  # 0: all selected rules pass, 2: at least one fails; anything else is an error.
  local rc=0
  oscap xccdf eval "$@" || rc=$?
  if [[ $rc -ne 0 && $rc -ne 2 ]]; then
    echo "oscap failed with exit code $rc" >&2
    exit "$rc"
  fi
}

section "Applying rules deferred during provisioning: ${STIG_BUILD_PHASE_RULES}"
rule_args=()
for r in ${STIG_BUILD_PHASE_RULES}; do rule_args+=(--rule "$r"); done
if [[ ${#rule_args[@]} -gt 0 ]]; then
  oscap_eval --remediate --profile "${STIG_PROFILE}" --tailoring-file "${STIG_TAILORING}" \
    "${rule_args[@]}" "${STIG_DATASTREAM}" >/dev/null
fi

section "Removing build-only fapolicyd allowances and the goss binary"
rm -f /etc/fapolicyd/rules.d/36-image-builder-build.rules
rm -rf /tmp/goss /tmp/goss-* /tmp/goss-spec.yaml /opt/image-builder
fagenrules --load
systemctl restart fapolicyd

section "Waiting for fapolicyd to enforce"
# fapolicyd forks, and systemd reports it active, before it loads its trust
# database from the RPM database and starts listening for events; until then
# it denies nothing. A bigger RPM database takes longer (a GPU build adds
# kernel-devel, the compiler toolchain and the NVIDIA driver), so wait until an
# untrusted executable is actually denied before testing anything under it.
# Not /tmp or /var/tmp: they are mounted noexec, which would deny it without
# fapolicyd.
denied=/opt/.stig-fapolicyd-probe
probes=()
created_dirs=()
cleanup_probes() {
  rm -f "${probes[@]}" "${denied}"
  for d in "${created_dirs[@]}"; do rmdir "${d}" 2>/dev/null || true; done
}
trap cleanup_probes EXIT
run_unprivileged() { LC_ALL=C setpriv --reuid=65534 --regid=65534 --clear-groups "$1"; }
# True only when fapolicyd denied the exec (EPERM), not for any other failure.
denied_by_fapolicyd() {
  local out
  if out="$(run_unprivileged "$1" 2>&1)"; then
    last_probe_output="(it ran)"
    return 1
  fi
  last_probe_output="${out}"
  [[ "${out}" == *"Operation not permitted"* ]]
}
cp /usr/bin/true "${denied}"
chmod 0755 "${denied}"
fapolicyd_timeout=600
last_probe_output="(it ran)"
for ((waited = 0; ; waited++)); do
  systemctl is-active --quiet fapolicyd || { echo "fapolicyd is not running" >&2; exit 1; }
  denied_by_fapolicyd "${denied}" && break
  if ((waited >= fapolicyd_timeout)); then
    echo "fapolicyd did not deny an untrusted executable within ${fapolicyd_timeout}s of starting; deny-by-default is not in effect. Last attempt: ${last_probe_output}" >&2
    journalctl -u fapolicyd -b --no-pager -n 20 >&2 || true
    exit 1
  fi
  sleep 1
done
echo "OK: fapolicyd denies untrusted executables (enforcing ${waited}s after restart)."

section "Checking the container runtime under fapolicyd"
# Earlier steps change containerd's drop-ins (sysprep removes the proxy one).
systemctl daemon-reload
systemctl restart containerd
CTR=/usr/local/bin/ctr
for _ in $(seq 1 30); do "${CTR}" version >/dev/null 2>&1 && break; sleep 1; done
# Start a container end to end (containerd -> shim -> runc -> container
# process) using kube-proxy from the pre-pulled control plane images.
image="$("${CTR}" -n k8s.io images ls -q | grep -E '/kube-proxy:' | head -1)"
if [[ -n "${image}" ]]; then
  if ! "${CTR}" -n k8s.io run --rm --net-host "${image}" stig-smoke-test /usr/local/bin/kube-proxy --version; then
    echo "A container could not start with fapolicyd enforcing. Recent fapolicyd denials:" >&2
    ausearch -m FANOTIFY -ts recent -i 2>/dev/null | grep -E "^type=(SYSCALL|PATH|FANOTIFY)" | tail -n 15 >&2 || true
    exit 1
  fi
else
  echo "No kube-proxy image found; skipping the container start test." >&2
fi
# An unprivileged process may run executables dropped at runtime into the
# directories 35-kubernetes.rules allows (CNI plugins, kubelet credential
# providers) but nothing untrusted elsewhere.
allowed_dirs=(/opt/cni/bin /etc/kubernetes/image-credential-provider)
for d in "${allowed_dirs[@]}"; do
  if [[ ! -d "${d}" ]]; then
    mkdir -p "${d}"
    created_dirs+=("${d}")
  fi
  probes+=("${d}/.stig-fapolicyd-probe")
done
for p in "${probes[@]}"; do
  cp /usr/bin/true "${p}"
  chmod 0755 "${p}"
done
for p in "${probes[@]}"; do
  run_unprivileged "${p}" ||
    { echo "fapolicyd blocks executables in ${p%/*}; Kubernetes would not run them" >&2; exit 1; }
done
if ! denied_by_fapolicyd "${denied}"; then
  echo "fapolicyd allowed an untrusted executable; deny-by-default is not in effect. Last attempt: ${last_probe_output}" >&2
  exit 1
fi
cleanup_probes
trap - EXIT
echo "OK: containers start, CNI plugin and credential provider directories allowed, untrusted executables denied."
# grow-var-lv runs at every node boot (stig-partitions.sh); there is no new
# space now, so this only proves fapolicyd lets it and its tools run.
if [[ -x /usr/local/sbin/grow-var-lv ]]; then
  /usr/local/sbin/grow-var-lv
  echo "OK: grow-var-lv runs under fapolicyd."
fi

# The only AIDE baseline the build makes: the STIG role leaves
# aide_build_database out of the provisioning pass (stig_build_phase_skip), so
# the whole filesystem is hashed once, after everything else has changed it.
section "Building the AIDE baseline"
aide --init >/dev/null
mv -f /var/lib/aide/aide.db.new.gz /var/lib/aide/aide.db.gz

section "Scanning the final image (${STIG_PROFILE})"
oscap_eval --profile "${STIG_PROFILE}" --tailoring-file "${STIG_TAILORING}" \
  --results "${REPORT_DIR}/results.xml" --report "${REPORT_DIR}/report.html" \
  "${STIG_DATASTREAM}" >/dev/null
chmod 0644 "${REPORT_DIR}"/*

section "STIG scan summary"
python3 - "${REPORT_DIR}/results.xml" <<'PY'
import collections, sys, xml.etree.ElementTree as ET
ns = {"x": "http://checklists.nist.gov/xccdf/1.2"}
root = ET.parse(sys.argv[1]).getroot()
rules = {}
for r in root.iter("{%s}Rule" % ns["x"]):
    vids = [ref.text for ref in r.findall("x:reference", ns) if (ref.text or "").startswith("V-")]
    title = r.find("x:title", ns)
    rules[r.get("id")] = (r.get("severity"), ",".join(vids), (title.text or "").strip() if title is not None else "")
tr = root.find(".//x:TestResult", ns)
counts = collections.Counter()
failed = []
for rr in tr.findall("x:rule-result", ns):
    res = rr.find("x:result", ns).text
    if res == "notselected":
        continue
    counts[res] += 1
    if res in ("fail", "error", "unknown"):
        sev, vid, title = rules.get(rr.get("idref"), ("", "", ""))
        failed.append((sev, vid, rr.get("idref").replace("xccdf_org.ssgproject.content_rule_", ""), title, res))
score = tr.find("x:score", ns)
print("Profile:   %s" % tr.find("x:profile", ns).get("idref"))
print("Score:     %.1f%%" % float(score.text) if score is not None else "")
print("Results:   " + ", ".join("%s=%d" % kv for kv in sorted(counts.items())))
cat = {"high": "CAT I", "medium": "CAT II", "low": "CAT III"}
order = {"high": 0, "medium": 1, "low": 2}
if failed:
    print("Open findings (need a site decision or a secret):")
    for sev, vid, rid, title, res in sorted(failed, key=lambda f: (order.get(f[0], 3), f[2])):
        print("  %-8s %-9s %-55s %s%s" % (cat.get(sev, sev), vid, rid, title[:70], "" if res == "fail" else " [%s]" % res))
print("Evidence in the image: /usr/share/image-builder/stig/{results.xml,report.html,tailoring.xml,exceptions.md}")
PY

# sudo_remove_nopasswd (applied above) comments out the build user's whole
# cloud-init sudoers line ("builder ALL=(ALL) NOPASSWD:ALL"), not just the
# NOPASSWD tag, so the user has no sudo left. Packer's shutdown_command needs
# sudo, with the password, to delete the user; it deletes this file as well.
# Written after the scan so the stored evidence doesn't include it.
section "Allowing password sudo for ${BUILD_USER} until shutdown deletes the user"
sudoers_file="/etc/sudoers.d/${BUILD_USER}"
# sudo skips files in sudoers.d with a "." in the name, so the temp file is inert.
printf '%s ALL=(ALL) ALL\n' "${BUILD_USER}" >"${sudoers_file}.tmp"
chmod 0440 "${sudoers_file}.tmp"
visudo -cf "${sudoers_file}.tmp" >/dev/null
mv -f "${sudoers_file}.tmp" "${sudoers_file}"
