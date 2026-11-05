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
                                   image "<prefix><os>-kube-<version>" in Prism Central
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
| Image name | `image_name` (also the build VM name) |
| Cluster / subnet | `nutanix_cluster_name` / `nutanix_subnet_name` |
| vCPU / memory / disk | `cpus` / `memory` / `disk_size_gb` |
| Source image: repo default | `image_url` from `packer/nutanix/<os>.json` (not offered for RHEL and Flatcar) |
| Source image: Prism Central image | `source_image_uuid` if the value is a UUID, otherwise `source_image_name`. The build boots from that existing image and never modifies or deletes it. |
| Source image: download URL | `image_url` |
| Proxies, extra packages | `http_proxy`, `https_proxy`, `no_proxy`, `extra_debs`, `extra_rpms` |
| Extra variables | any other variable in `packer/nutanix/variables.pkr.hcl` except credentials, SSH, naming and custom ansible/post-processor hooks |
| Credential override | per-build Secret: `NUTANIX_ENDPOINT/PORT/INSECURE/USERNAME/PASSWORD`, `RHSM_USER/PASS` |

The rendered var-file is shown in each build's detail view.

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
nkp create cluster nutanix --vm-image nkp-ubuntu-2204-kube-v1.34.2 --kubernetes-version 1.34.2 …
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
