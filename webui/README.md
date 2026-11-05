# NKP Image Builder web UI

A small web service, deployed with Helm, that lets users fill out a form and
build a Kubernetes node image on Nutanix AHV using this repository's
`images/capi` Packer + Ansible pipeline. The finished image is registered in
Prism Central, ready for use with Nutanix Kubernetes Platform (NKP).

```
 browser ──► webui (Deployment) ──creates──► Job: nkp-ib-<id>
                                              │  image: images/capi/Dockerfile
                                              │  cmd:   make build-nutanix-<os>
                                              │  env:   NUTANIX_* from Secret
                                              │  vars:  ConfigMap → PACKER_VAR_FILES
                                              ▼
                                   Prism Central API ── build VM on AHV
                                              │  (SSH from pod → ansible + goss)
                                              ▼
                                   image "nkp-<os>-release-<version>-<ts>.qcow2" in PC
```

* **No state of its own.** Each build is a Kubernetes Job. Its form inputs (with
  secrets removed) are stored as a Job annotation, and its logs are the pod's
  logs. Finished Jobs are cleaned up after `builder.ttlSecondsAfterFinished`.
* **Unmodified build pipeline.** The Job runs the same `make build-nutanix-<os>`
  target you would run by hand. The form is rendered into a Packer var-file that
  the Makefile appends last through `PACKER_VAR_FILES`, so form values override
  `packer/config/*.json` and `packer/nutanix/<os>.json`.
* **Cancelling cleans up.** Cancelling a build deletes the Job. The pod's
  SIGTERM is turned into a SIGINT for the whole `make`/`packer` process group
  (the same as Ctrl-C), and Packer gets `builder.terminationGracePeriodSeconds`
  to delete the build VM in Prism Central.

## What the form sets

| Form field | Packer variable(s) |
|---|---|
| Operating system | make target `build-nutanix-<id>` (`packer/nutanix/<id>.json`) |
| Kubernetes version `v1.X.Y` | `kubernetes_semver`, `kubernetes_series=v1.X`, `kubernetes_deb_version=1.X.Y-1.1`, `kubernetes_rpm_version=1.X.Y`, `crictl_version=1.X.0` |
| Image name | `image_name` (also the build VM name). Blank uses `defaults.imageNameTemplate`, by default `nkp-<os>-release-<version>-<UTC timestamp>.qcow2`, e.g. `nkp-ubuntu-24.04-release-1.36.1-20261105143022.qcow2` |
| Cluster / subnet | `nutanix_cluster_name` / `nutanix_subnet_name` |
| vCPU / memory / disk | `cpus` / `memory` / `disk_size_gb` |
| Source image: repo default | `image_url` from `packer/nutanix/<os>.json` (not offered for RHEL and Flatcar) |
| Source image: Prism Central image | `source_image_uuid` if the value is a UUID, otherwise `source_image_name`. The build boots from that existing image and never modifies or deletes it. |
| Source image: download URL | `image_url` |
| DISA STIG hardening (RHEL 9, Rocky 9) | `stig_enabled=true` (implies FIPS), `stig_extra_exceptions` and `stig_remote_syslog` from the chart. See [DISA STIG images](#disa-stig-images). |
| FIPS 140 mode (RHEL 9, Rocky 9) | `fips_enabled=true` plus the `fips.*` sources from the chart. See [FIPS images](#fips-images). |
| Proxies, extra packages | `http_proxy`, `https_proxy`, `no_proxy`, `extra_debs`, `extra_rpms` |
| Extra variables | any other variable in `packer/nutanix/variables.pkr.hcl` except credentials, SSH, naming and custom ansible/post-processor hooks |
| Credential override | per-build Secret: `NUTANIX_ENDPOINT/PORT/INSECURE/USERNAME/PASSWORD`, `RHSM_USER/PASS` |

The rendered var-file is shown in each build's detail view.

## FIPS images

Ticking **FIPS 140 mode** (offered for OSes with `fipsCapable: true`, by
default RHEL 9 and Rocky Linux 9) builds an image that boots in FIPS mode and
runs only FIPS-validated crypto on the Kubernetes path:

* **Kernel and OS.** Before provisioning, the build runs `fips-mode-setup
  --enable` (`fips=1` on every kernel command line, the dracut `fips` module,
  and the system-wide `FIPS` crypto policy for OpenSSL, GnuTLS, NSS and
  OpenSSH). It then reboots, so the rest of the build runs on the FIPS kernel.
* **Kubernetes.** kubelet, kubeadm, kubectl, cri-tools and kubernetes-cni come
  from the Nutanix/D2iQ FIPS RPM repo (GPG-checked). These are the same
  BoringCrypto builds NKP's FIPS clusters use. The `kube-*`, etcd and pause
  images are pre-pulled as `docker.io/mesosphere/*:vX.Y.Z_fips.0`.
* **Runtime.** containerd, its shim and runc come from the FIPS containerd
  bundle (BoringCrypto), pinned by sha256.
* **Verification.** goss fails the build unless:
  * `/proc/sys/crypto/fips_enabled` is 1
  * every boot entry has `fips=1`
  * the crypto policy is FIPS
  * the initramfs has the fips module
  * kubelet, kubeadm, kubectl, containerd, the shim and runc are all FIPS
    (BoringCrypto/GOFIPS140) builds
* **Naming.** Default image names gain `-fips`, e.g.
  `nkp-rockylinux-9-fips-release-1.36.1-<timestamp>.qcow2`.

Not every Kubernetes patch release has a FIPS build. Before a build starts, the
service checks that the FIPS repo has the requested version, and rejects it
otherwise (`fips.preflight`). Create clusters from the image with the FIPS
control plane. The build log prints the etcd version under "FIPS etcd image":

```sh
nkp create cluster nutanix --vm-image nkp-rockylinux-9-fips-release-1.36.1-… \
  --kubernetes-version v1.36.1+fips.0 --kubernetes-image-repository docker.io/mesosphere \
  --etcd-version 3.6.8+fips.0 --etcd-image-repository docker.io/mesosphere …
```

For air-gapped sites, point the `fips.*` values at mirrors.
`fips.kubernetesRpmRepo` may contain `{version}`. Users can't override FIPS
sources through extra variables.

Ubuntu FIPS isn't offered. It needs an Ubuntu Pro token for the FIPS kernel,
and there is no FIPS-built Kubernetes `.deb` source to install from.

## DISA STIG images

**DISA STIG hardening** (OSes with `stigCapable: true`) always produces a FIPS
image, because FIPS mode is itself a STIG requirement (V-258230, CAT I). On
top of that:

1. **Remediation.** Near the end of provisioning, the `stig` Ansible role
   applies the SCAP Security Guide **DISA STIG profile** for the distribution
   (`ssg-rhel9-ds.xml` on RHEL, `ssg-rl9-ds.xml` on Rocky) with `oscap
   --remediate`. It uses a tailoring file that deselects only the rules a
   Kubernetes node can't meet:

   | Rule | Vuln ID | Why |
   |---|---|---|
   | IPv4/IPv6 forwarding off | V-257970, V-257974 | Pods are routed through the node; kubeadm requires `ip_forward=1`. |
   | firewalld active, default deny, ssh port | V-257936, V-257937, V-257940 | The CNI (Cilium) owns the node's packet filtering; firewalld rewriting the same tables breaks pod networking. |
   | nfs-utils removed | V-257828 | kubelet needs `mount.nfs` for NFS PersistentVolumes (Nutanix Files). |
   | user namespaces disabled | V-257816 | Container runtimes and `hostUsers: false` pods use them. The STIG allows this when it's documented. |

   Everything else is enforced, including fapolicyd deny-by-default,
   usbguard, auditd, AIDE, the DoD login banner, SSH and PAM settings, and the
   FIPS:STIG crypto policy. The non-RPM containerd/runc binaries are added to
   the fapolicyd trust database, and `/opt/cni/bin` is allowed so CNI plugins
   installed at runtime can run.
2. **Final step**, after goss:
   * applies the last sudo rule (the build user is removed at shutdown)
   * starts a container through containerd, the shim and runc with
     fapolicyd enforcing
   * checks that untrusted executables are denied and CNI plugins are allowed
   * rebuilds the AIDE baseline
   * scans the finished image
3. **Evidence.** `results.xml`, `report.html`, `tailoring.xml` and
   `exceptions.md` (each exception with its justification, for your ISSM) are
   kept in the image at `/usr/share/image-builder/stig/`. The end of the build
   log lists the score and every open finding by CAT level.

Some findings need a site decision or a different disk image, so expect them
in the report until you address them:

* **Separate partitions** for `/var`, `/var/log`, `/var/log/audit`, `/tmp`,
  `/var/tmp` and `/home`. Cloud images have a single partition. Build from a
  Prism Central source image that was installed with STIG partitioning (see
  *Source image*).
* **GRUB password** (V-257787, CAT I). Add `STIG_GRUB_PASSWORD_HASH` (from
  `grub2-mkpasswd-pbkdf2`) to the Nutanix credentials Secret.
* **Remote log host** (V-258149). Set `stig.remoteSyslog`.
* **Disk encryption** (V-257879, CAT I). If your ISSM accepts Nutanix
  Data-at-Rest Encryption, add it to `stig.extraExceptions` with that
  justification.
* **Rocky Linux** always fails "vendor-supported OS" (V-257777, CAT I).
  DISA publishes the STIG for RHEL 9 only, so use RHEL 9 for an
  accreditation baseline.

The image covers the operating system STIG. The DISA **Kubernetes** STIG
(API server, kubelet configuration, and so on) applies to the cluster NKP
creates, not to the node image.

## Prerequisites

* A Kubernetes cluster to run the service, such as the NKP management cluster.
* A registry the cluster can pull from.
* Network access from the build pods:
  * Prism Central on 9440.
  * **SSH (22) to the build VM's subnet.** Ansible connects to the VM directly
    from the pod.
  * The internet, or your mirrors/proxy, for pkgs.k8s.io, cloud images, the
    goss binary, and so on.
* A Prism Central user that can create and delete VMs and images on the target
  cluster.

## Build and publish the images

```sh
cd webui
make push REGISTRY=registry.example.com/nkp TAG=v0.1.0
```

This builds two images:

* `nkp-image-builder-webui`: this service (distroless, about 20 MB).
* `nkp-image-builder`: the job image from `../images/capi/Dockerfile`, with
  Packer, Ansible and the Nutanix Packer plugin preinstalled.

## Install

```yaml
# my-values.yaml
image:
  repository: registry.example.com/nkp/nkp-image-builder-webui
  tag: v0.1.0
builder:
  image:
    repository: registry.example.com/nkp/nkp-image-builder
    tag: v0.1.0
nutanix:
  existingSecret: prism-central-creds   # or fill nutanix.credentials.*
defaults:
  clusterName: pe-cluster-01
  subnetName: build-subnet
catalog:
  kubernetesVersions: [v1.34.2, v1.33.5]  # what your NKP release supports
auth:
  basic:
    enabled: true
    password: change-me
ingress:
  enabled: true
  className: nginx
  annotations:
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
  hosts:
    - host: image-builder.example.com
      paths: [{path: /, pathType: Prefix}]
```

```sh
kubectl create namespace nkp-image-builder
kubectl -n nkp-image-builder create secret generic prism-central-creds \
  --from-literal=NUTANIX_ENDPOINT=pc.example.com \
  --from-literal=NUTANIX_PORT=9440 \
  --from-literal=NUTANIX_INSECURE=false \
  --from-literal=NUTANIX_USERNAME=svc-image-builder \
  --from-literal=NUTANIX_PASSWORD='…'

helm upgrade --install nkp-image-builder charts/nkp-image-builder \
  -n nkp-image-builder -f my-values.yaml
```

See [values.yaml](charts/nkp-image-builder/values.yaml) for every option,
including the OS catalog, build resources, node selectors, TTL and timeouts.

## Use the image in NKP

When a build succeeds, its image name appears in Prism Central → Images. Pass
that name when you create a cluster:

```sh
nkp create cluster nutanix --vm-image nkp-ubuntu-22.04-release-1.34.2-20261105143022.qcow2 --kubernetes-version 1.34.2 …
```

Keep the image's Kubernetes version within the versions your NKP release
supports.

## Security notes

* Anyone who can reach the UI can start builds with the configured Prism
  Central credentials. Enable `auth.basic`, or put the UI behind an
  authenticating proxy (oauth2-proxy, an ingress with SSO).
* The UI's ServiceAccount can create, update and delete Secrets in its namespace
  (for per-build credential overrides), but it cannot read or list them.
* Build pods run as non-root with all capabilities dropped and no API token.
* Extra Packer variables go through a deny-list that blocks credentials, SSH
  settings, `custom_post_processor_command` and custom ansible roles. Set
  `features.allowExtraVars: false` to turn off free-form variables entirely.
* POST requests must be `application/json`, and cross-origin requests are
  rejected (CSRF protection).

## Develop

```sh
make test                       # unit tests (uses client-go fake clientset)
make run                        # run against current kubeconfig using hack/dev-config.json
make lint-chart
```

## Limitations

* Windows builds are not offered. The Windows Nutanix flow needs a separate
  unattend/ISO step and different credentials.
* Build history lasts only as long as the Jobs. Logs are gone after the TTL.
* The service builds one image per Job in the release namespace. There is no
  queueing; use `builder.resources` and namespace quotas to limit concurrency.
