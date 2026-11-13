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
| Kubernetes version `v1.X.Y` | `kubernetes_semver`, `kubernetes_series=v1.X`, `kubernetes_deb_version=1.X.Y` (the build installs the newest package revision, e.g. `1.36.2-2.1`), `kubernetes_rpm_version=1.X.Y`, `crictl_version=1.X.0` |
| Image name | `image_name` (also the build VM name). Blank uses `defaults.imageNameTemplate`, by default `nkp-<os>[-fips][-stig][-gpu\|-vgpu]-release-<version>-<UTC timestamp>.qcow2`, e.g. `nkp-ubuntu-24.04-release-1.36.1-20261105143022.qcow2` |
| Cluster / subnet | `nutanix_cluster_name` / `nutanix_subnet_name` |
| vCPU / memory / disk | `cpus` / `memory` / `disk_size_gb` |
| Source image: repo default | `image_url` from `packer/nutanix/<os>.json` (not offered for RHEL and Flatcar) |
| Source image: Prism Central image | `source_image_uuid` if the value is a UUID, otherwise `source_image_name`. The build boots from that existing image and never modifies or deletes it. |
| Source image: download URL | `image_url` |
| DISA STIG hardening (RHEL 9, Rocky 9) | `stig_enabled=true` (implies FIPS), `stig_extra_exceptions` and `stig_remote_syslog` from the chart. See [DISA STIG images](#disa-stig-images). |
| FIPS 140 mode (RHEL 9, Rocky 9) | `fips_enabled=true` plus the `fips.*` sources from the chart. See [FIPS images](#fips-images). |
| Attach a GPU (Ubuntu, RHEL 9, Rocky 9) | `nutanix_gpu_name`, plus the `gpu.nvidia.*` driver settings from the chart. See [GPU images](#gpu-images). |
| Proxies, extra packages | `http_proxy`, `https_proxy`, `no_proxy`, `extra_debs`, `extra_rpms` |
| Extra variables | any other variable in `packer/nutanix/variables.pkr.hcl` except credentials, SSH, naming and custom ansible/post-processor hooks |
| Credential override | per-build Secret: `NUTANIX_ENDPOINT/PORT/INSECURE/USERNAME/PASSWORD`, `RHSM_USER/PASS` |

The rendered var-file is shown in each build's detail view.

The form doesn't set the container runtime. Every image gets the same
containerd as NKP's own node images, whatever the Kubernetes version:
upstream containerd 2.3.1 and runc 1.4.2 (`packer/config/containerd.json`).
FIPS images get the FIPS 2.3.1 bundle instead (see [FIPS images](#fips-images)).
An extra `containerd_version` must be one the bundled `containerd.service`
supports (`containerd_service_template_versions` in the containerd role,
currently 2.3.1 and 2.3.2), or the build fails.

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

## GPU images

Ticking **Attach a GPU** (offered for OSes with `gpuCapable: true`: Ubuntu,
RHEL 9 and Rocky Linux 9) builds a node image with the NVIDIA driver baked in,
checked against a real GPU:

* **Picking the GPU.** The form lists the GPUs and vGPU profiles on the build
  cluster's hosts in Prism Central, with how many are free. Pick one that is
  free; the Nutanix Packer plugin attaches it to the build VM by name
  (`nutanix_gpu_name`). Before a build starts, the service checks the GPU is
  on that cluster, is NVIDIA, and has one free. With `gpu.discovery: false`,
  or when Prism Central can't be reached, type the name instead.
* **Driver.**
  * *Passthrough GPUs* get NVIDIA's data center driver, branch
    `gpu.nvidia.driverBranch` (default 580, a long-term support branch), with
    the open kernel modules (set `openKernelModules: false` for P4, P40, P100
    and V100). Ubuntu installs it from the Ubuntu archive with `ubuntu-drivers
    --gpgpu`, using Canonical's signed, prebuilt kernel modules. RHEL/Rocky
    install the `nvidia-driver:<branch>-open` module stream's compute-only
    packages from NVIDIA's CUDA repo (`gpu.nvidia.rpmRepo` to mirror it) and
    build the module with DKMS (from EPEL).
  * *vGPU profiles* get NVIDIA's vGPU guest driver. Only the NVIDIA Licensing
    Portal distributes it, so put it on a mirror and set
    `gpu.nvidia.vgpuDriver` (a `.run` for every OS, or an `.rpm` for RHEL/Rocky
    and a `.deb` for Ubuntu). Its version must be supported by the AHV hosts'
    vGPU manager. To license vGPUs from the image, add an
    `NVIDIA_VGPU_LICENSE_TOKEN` key (the NLS client configuration token) to the
    Nutanix credentials Secret; it is written to
    `/etc/nvidia/ClientConfigToken/` and `nvidia-gridd` is enabled.
* **Order of the build.** After the FIPS/STIG preparation the build upgrades
  the OS, blocks nouveau and reboots into the newest kernel
  (`ansible/gpu-prepare.yml`), then checks the GPU is on the PCI bus. Next it
  installs the driver for the kernel the image boots, loads it, and fails
  unless `nvidia-smi` sees the GPU (`roles/gpu`, `install.yml`). goss checks
  `nvidia-smi` again at the end. `nvidia-persistenced` is enabled.
* **DISA STIG.** Driver files must come from packages, because fapolicyd
  trusts only packaged files. The data center driver always does; vGPU STIG
  builds need `vgpuDriver.rpm`.
* **UEFI.** GPUs with large BARs (A100, H100, ...) need UEFI. Add
  `boot_type=uefi` to the extra variables (Rocky Linux 9 already uses UEFI),
  and boot the nodes with UEFI too.
* **Naming.** Default image names gain `-gpu` or `-vgpu`, e.g.
  `nkp-ubuntu-24.04-gpu-release-1.36.1-<timestamp>.qcow2`.

Flatcar isn't offered. Its NVIDIA drivers are systemd-sysext images (see
`ansible/roles/gpu/README.md`), not something the build can install and check
this way.

To use the image, give the GPU workers the same GPU in their
`NutanixMachineTemplate` (`spec.template.spec.gpus: [{type: name, name:
"<GPU name>"}]`). The driver is already on the node, so deploy the NVIDIA GPU
Operator with `driver.enabled=false`. It still installs the container toolkit
and device plugin.

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
   | firewalld active, default deny, ssh port | V-257936, V-257937, V-257940 | The CNI (Cilium) owns the node's packet filtering; firewalld rewriting the same tables breaks pod networking, and its default zone blocks kubelet, API server and CNI traffic. firewalld is installed and configured but masked. |
   | nfs-utils removed | V-257828 | kubelet needs `mount.nfs` for NFS PersistentVolumes (Nutanix Files). |
   | gssproxy removed | V-257832 | nfs-utils depends on gssproxy, so removing it also removes nfs-utils. |
   | user namespaces disabled | V-257816 | Container runtimes and `hostUsers: false` pods use them. The STIG allows this when it's documented. |

   Everything else is enforced, including fapolicyd deny-by-default,
   usbguard, auditd, AIDE, the DoD login banner, SSH and PAM settings, and the
   FIPS:STIG crypto policy. The non-RPM containerd/runc binaries are added to
   the fapolicyd trust database, and `/opt/cni/bin` is allowed so CNI plugins
   installed at runtime can run.
   **Disk layout.** The STIG requires separate filesystems for `/var`,
   `/var/log`, `/var/log/audit`, `/var/tmp`, `/home` and `/tmp`. Cloud
   images have a single XFS root partition, and XFS can't shrink. So a STIG
   build stops cloud-init from growing the root partition and puts an LVM
   volume group (`vg_node`) in the rest of the build disk before the FIPS
   reboot:

   | Mount | Size | STIG mount options (set by the remediation) |
   |---|---|---|
   | `/var/log` | 8 GiB (kubelet keeps container logs in `/var/log/pods`) | nodev, nosuid, noexec |
   | `/var/log/audit` | 4 GiB | nodev, nosuid, noexec |
   | `/var/tmp` | 2 GiB | nodev, nosuid, noexec |
   | `/home` | 1 GiB | nodev, nosuid, noexec |
   | `/tmp` | 2 GiB | nodev, nosuid, noexec |
   | `/var` | the rest, at least 10 GiB | nodev |

   * **Disk size.** STIG builds need a disk of at least 40 GB.
   * **Nodes.** containerd, kubelet and etcd data lives on `/var`. Nodes are
     cloned onto disks larger than the image, and `grow-var-lv.service`
     grows `/var` into the extra space at boot.
   * **Changing the sizes.** Change the Packer variable `stig_partition_sizes`.
   * **Already-partitioned source image.** If the Prism Central source image
     already has these mounts (for example, one installed with a STIG
     kickstart), the build leaves them as they are.
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
* Network access from the UI pod to Prism Central on 9440, if GPU discovery
  is on (`gpu.discovery`).
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
* With GPU discovery on (`gpu.discovery`, the default when `gpu.enabled`), the
  UI pod gets the Prism Central credentials as environment variables, to list
  the build cluster's GPUs. Turn discovery off to keep them out of the UI pod;
  users then type the GPU name.
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
