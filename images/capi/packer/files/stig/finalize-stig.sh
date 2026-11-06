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
for _ in $(seq 1 30); do systemctl is-active --quiet fapolicyd && break; sleep 1; done

section "Checking the container runtime under fapolicyd"
systemctl is-active --quiet fapolicyd || { echo "fapolicyd is not running" >&2; exit 1; }
systemctl restart containerd
CTR=/usr/local/bin/ctr
for _ in $(seq 1 30); do "${CTR}" version >/dev/null 2>&1 && break; sleep 1; done
# Start a container end to end (containerd -> shim -> runc -> container
# process) using kube-proxy from the pre-pulled control plane images.
image="$("${CTR}" -n k8s.io images ls -q | grep -E '/kube-proxy:' | head -1)"
if [[ -n "${image}" ]]; then
  "${CTR}" -n k8s.io run --rm --net-host "${image}" stig-smoke-test /usr/local/bin/kube-proxy --version
else
  echo "No kube-proxy image found; skipping the container start test." >&2
fi
# An unprivileged process may run a plugin dropped into /opt/cni/bin at
# runtime (35-kubernetes.rules) but nothing untrusted elsewhere.
probe=/opt/cni/bin/.stig-fapolicyd-probe
cp /usr/bin/true "${probe}"
# Not /tmp or /var/tmp: they are mounted noexec, which would deny it without
# fapolicyd.
denied=/opt/.stig-fapolicyd-probe
cp /usr/bin/true "${denied}"
chmod 0755 "${probe}" "${denied}"
trap 'rm -f "${probe}" "${denied}"' EXIT
setpriv --reuid=65534 --regid=65534 --clear-groups "${probe}" ||
  { echo "fapolicyd blocks executables in /opt/cni/bin; CNI plugins would not run" >&2; exit 1; }
if setpriv --reuid=65534 --regid=65534 --clear-groups "${denied}" 2>/dev/null; then
  echo "fapolicyd allowed an untrusted executable; deny-by-default is not in effect" >&2
  exit 1
fi
rm -f "${probe}" "${denied}"
trap - EXIT
echo "OK: containers start, CNI plugin directory allowed, untrusted executables denied."
# grow-var-lv runs at every node boot (stig-partitions.sh); there is no new
# space now, so this only proves fapolicyd lets it and its tools run.
if [[ -x /usr/local/sbin/grow-var-lv ]]; then
  /usr/local/sbin/grow-var-lv
  echo "OK: grow-var-lv runs under fapolicyd."
fi

section "Rebuilding the AIDE baseline"
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
