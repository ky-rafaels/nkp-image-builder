// Variable declarations for the Nutanix Packer template.
//
// This template is still fed by the shared packer/config/*.json var-files via
// -var-file (see the Makefile), the same way every other, still-JSON,
// provider is. Those files are plain JSON key/value maps, which HCL2 accepts
// fine as a var-file -- so most of the declarations below just mirror the
// keys those shared files set. Values derived from OTHER variables (e.g. a
// Kubernetes repo URL built from `kubernetes_series`) can't be expressed as a
// variable default in HCL2 (only constant expressions/`env()` are allowed),
// so those live in locals.pkr.hcl instead.
//
// Variables referenced by a downstream var-file/CI pipeline keep their
// original legacy-JSON names on purpose (renaming them would silently break
// any existing var-file that overrides them -- this bit a downstream
// consumer during development). nutanix_port/nutanix_insecure are declared
// with their real number/bool types rather than the legacy string -- a JSON
// var-file or -var flag setting e.g. "9440"/"true" auto-converts fine, so
// there's no behavior loss for that path. `env()` can't produce a non-empty
// number/bool default directly, so the legacy NUTANIX_PORT/NUTANIX_INSECURE
// env-var fallback is wired up via the *_env string variables below and
// local.nutanix_port/local.nutanix_insecure in locals.pkr.hcl (used by
// sources.pkr.hcl instead of the raw variables).

##########################
# Nutanix connection/API #
##########################

variable "nutanix_endpoint" {
  type        = string
  description = "Nutanix Prism Central endpoint (IP or hostname)."
  default     = env("NUTANIX_ENDPOINT")
}
variable "nutanix_username" {
  type        = string
  description = "Nutanix Prism Central username."
  default     = env("NUTANIX_USERNAME")
}
variable "nutanix_password" {
  type        = string
  description = <<-EOT
    Nutanix Prism Central password.

    NOTE: if you override this via -var-file, the value must be a real,
    already-resolved string -- HCL2 var-files are static data, not templates,
    so a legacy-JSON-style "{{env `PASSWORD`}}" placeholder will be taken
    literally (and will break authentication) instead of being resolved.
    Pass the real secret directly, or use `-var nutanix_password=...`.
  EOT
  default     = env("NUTANIX_PASSWORD")
  sensitive   = true
}
variable "nutanix_port" {
  type        = number
  description = "Nutanix Prism Central API port. Leave unset (null) to fall back to the NUTANIX_PORT env var, then to the plugin's own default."
  default     = null
}
variable "nutanix_port_env" {
  type        = string
  description = "Fallback for nutanix_port, sourced from the legacy NUTANIX_PORT env var; see local.nutanix_port in locals.pkr.hcl."
  default     = env("NUTANIX_PORT")
}
variable "nutanix_insecure" {
  type        = bool
  description = "Whether to skip TLS certificate verification against Prism Central. Leave unset (null) to fall back to the NUTANIX_INSECURE env var, then to the plugin's own default."
  default     = null
}
variable "nutanix_insecure_env" {
  type        = string
  description = "Fallback for nutanix_insecure, sourced from the legacy NUTANIX_INSECURE env var; see local.nutanix_insecure in locals.pkr.hcl."
  default     = env("NUTANIX_INSECURE")
}
variable "nutanix_cluster_name" {
  type        = string
  description = "Name of the Nutanix cluster to build on."
  default     = env("NUTANIX_CLUSTER_NAME")
}
variable "nutanix_subnet_name" {
  type        = string
  description = "Name of the Nutanix subnet to attach the build VM's NIC to."
  default     = env("NUTANIX_SUBNET_NAME")
}

#####################
# VM sizing/lifecycle #
#####################

variable "cpus" {
  type        = number
  description = "Number of vCPUs for the build VM."
  default     = 1
}
variable "memory" {
  type        = number
  description = "Memory (MB) for the build VM."
  default     = 2048
}
variable "disk_size_gb" {
  type        = number
  description = "Boot disk size (GB) for the build VM."
  default     = 10
}
variable "force_deregister" {
  type        = bool
  description = "Deregister (delete) any pre-existing image with the same name before building."
  default     = true
}
variable "image_delete" {
  type        = bool
  description = "Delete the resulting Nutanix image if the build fails."
  default     = false
}
variable "image_export" {
  type        = bool
  description = "Export the resulting image after the build."
  default     = false
}
variable "source_image_delete" {
  type        = bool
  description = "Delete the downloaded source disk image after the build VM is created."
  default     = false
}
variable "source_image_force" {
  type        = bool
  description = "Force re-download of the source disk image even if a matching one already exists."
  default     = false
}
variable "vm_force_delete" {
  type        = bool
  description = "Force-delete the build VM (skip graceful shutdown) during cleanup."
  default     = true
}
variable "ssh_username" {
  type        = string
  description = "SSH user Packer connects as during provisioning."
  default     = "builder"
}

# Generated fresh by hack/set-ssh-password.sh (via the `set-ssh-password` Make
# prerequisite) into packer/ssh-password.auto.pkrvars.json, since the env var
# exported there doesn't survive into the separate shell that runs the
# actual packer build/validate recipe line -- only the filesystem does.
variable "ssh_password" {
  type        = string
  description = "SSH password for the build user, freshly generated per-build by hack/set-ssh-password.sh."
  default     = ""
  sensitive   = true
}

# The resulting image/VM name. Derived from build_name + kubernetes_semver by
# default (see locals.pkr.hcl) but stays overridable via -var/-var-file for
# callers (e.g. a downstream CI pipeline) that want their own naming
# convention -- pass a real, already-resolved string, not a template.
variable "image_name" {
  type        = string
  description = "Full name for the resulting Nutanix image/VM. Leave empty to use the default `<build_name>-kube-<kubernetes_semver>` pattern."
  default     = ""
}

#############################################
# Never set anywhere in this template today  #
#############################################

variable "boot_type" {
  type        = string
  description = "VM boot type (e.g. `legacy`/`uefi`). Unset by any current var-file; harmless no-op if left blank."
  default     = ""
}
variable "custom_post_processor" {
  type        = string
  description = "Set to `true` to run custom_post_processor_command after the build."
  default     = ""
}
variable "custom_post_processor_command" {
  type        = string
  description = "Shell command to run when custom_post_processor is `true`."
  default     = ""
}
variable "pypy_http_source" {
  type        = string
  description = "PyPy download source, passed through to the flatcar bootstrap script's environment. Unset by any current var-file; harmless no-op if left blank."
  default     = ""
}

variable "ansible_extra_vars" {
  type        = string
  description = "Extra --extra-vars payload appended to the ansible provisioner, on top of the common one built in locals.pkr.hcl."
  default     = ""
}

##########################################################################
# Populated by the Makefile's jq step (base64-encoded per-OS cloud-init  #
# user-data) via -var-file before every build/validate.                 #
##########################################################################

variable "user_data" {
  type        = string
  description = "Base64-encoded cloud-init/ignition user-data for the VM's initial boot, injected by the Makefile."
  default     = ""
}

##########################################################
# Supplied via the per-OS var-file (e.g. ubuntu-2204.json) #
##########################################################

variable "build_name" {
  type        = string
  description = "Short name identifying the OS/build variant (e.g. `ubuntu-2204`)."
}
variable "distro_name" {
  type        = string
  description = "Distro identifier used for goss's OS test variable and the ansible_common_vars payload (e.g. `ubuntu`, `rhel`)."
}
variable "distribution_version" {
  type        = string
  description = "Distro version used for goss's OS_VERSION test variable (e.g. `22.04`). Not set by the ubuntu var-files (blank is fine there)."
  default     = ""
}
variable "guest_os_type" {
  type        = string
  description = "Guest OS type as understood by the Nutanix builder (`Linux`)."
}
variable "image_url" {
  type        = string
  description = "URL (or path) of the source disk image to boot from. Ignored when source_image_name or source_image_uuid is set."
}
variable "source_image_name" {
  type        = string
  description = "Name of an image that already exists in Prism Central to boot the build VM from, instead of downloading image_url. Must be unique in Prism Central; use source_image_uuid otherwise."
  default     = ""
}
variable "source_image_uuid" {
  type        = string
  description = "UUID of an image that already exists in Prism Central to boot the build VM from. Takes precedence over source_image_name and image_url."
  default     = ""
}
variable "shutdown_command" {
  type        = string
  description = "Command used to shut the VM down cleanly at the end of the build."
}

###############################################################################
# Everything below mirrors the shared packer/config/*.json var-files that get #
# stacked onto every provider's build via -var-file in the Makefile           #
# ($(PACKER_NODE_FLAGS)). They're declared here (rather than shared/symlinked #
# across providers) since this migration is scoped to nutanix only for now.  #
# Nothing here has a provider-specific meaning; most of it just feeds the    #
# composed ansible_common_vars string built in locals.pkr.hcl.               #
###############################################################################

# --- packer/config/kubernetes.json ---
variable "crictl_version" {
  type    = string
  default = "1.36.0"
}
variable "kubeadm_template" {
  type    = string
  default = "etc/kubeadm.yml"
}
variable "kubernetes_apiserver_port" {
  type    = string
  default = "6443"
}
variable "kubernetes_container_registry" {
  type    = string
  default = "registry.k8s.io"
}
variable "kubernetes_deb_version" {
  type    = string
  default = "1.36.1-1.1"
}
variable "kubernetes_http_source" {
  type    = string
  default = "https://dl.k8s.io/release"
}
variable "kubernetes_load_additional_imgs" {
  type    = string
  default = "false"
}
variable "kubernetes_rpm_gpg_check" {
  type    = string
  default = "True"
}
variable "kubernetes_rpm_version" {
  type    = string
  default = "1.36.1"
}
variable "kubernetes_semver" {
  type        = string
  description = "Full Kubernetes version to install (e.g. `v1.36.1`)."
  default     = "v1.36.1"
}
variable "kubernetes_series" {
  type        = string
  description = "Kubernetes minor-version series used to build the pkgs.k8s.io repo URLs (e.g. `v1.36`)."
  default     = "v1.36"
}
variable "kubernetes_source_type" {
  type    = string
  default = "pkg"
}
variable "systemd_prefix" {
  type    = string
  default = "/usr/lib/systemd"
}
variable "sysusr_prefix" {
  type    = string
  default = "/usr"
}
variable "sysusrlocal_prefix" {
  type    = string
  default = "/usr/local"
}

# --- packer/config/cni.json ---
variable "kubernetes_cni_deb_version" {
  type    = string
  default = ""
}
variable "kubernetes_cni_http_source" {
  type    = string
  default = "https://github.com/containernetworking/plugins/releases/download"
}
variable "kubernetes_cni_rpm_version" {
  type    = string
  default = ""
}
variable "kubernetes_cni_semver" {
  type    = string
  default = "v1.2.0"
}
variable "kubernetes_cni_source_type" {
  type    = string
  default = "pkg"
}

# --- packer/config/containerd.json ---
variable "containerd_additional_settings" {
  type    = string
  default = ""
}
variable "containerd_cri_socket" {
  type    = string
  default = "/var/run/containerd/containerd.sock"
}
variable "containerd_enable_limit_no_file" {
  type    = string
  default = "false"
}
variable "containerd_gvisor_runtime" {
  type    = string
  default = "false"
}
variable "containerd_gvisor_version" {
  type    = string
  default = "latest"
}
variable "containerd_image_pull_progress_timeout" {
  type    = string
  default = ""
}
variable "containerd_version" {
  type        = string
  description = "containerd version to install."
  default     = "2.3.2"
}
variable "runc_version" {
  type    = string
  default = "1.4.3"
}
# Real default lives in packer/config/ppc64le/containerd.json; empty on other archs.
variable "containerd_sha256" {
  type    = string
  default = ""
}

# --- packer/config/wasm-shims.json ---
# The two vars below hold literal JSON blobs (that's what the ansible role on
# the other end expects). A `variable` default can only be a literal
# expression -- jsonencode() isn't allowed there -- so the readable map lives
# in locals.pkr.hcl instead, with these left as an empty-string override point
# (same pattern as image_name above) for anyone who wants to supply their own
# encoded blob via -var/-var-file.
variable "containerd_wasm_shims_runtime_versions" {
  type        = string
  description = "JSON-encoded map of wasm shim runtime versions. Leave empty to use the default set in locals.pkr.hcl."
  default     = ""
}
variable "containerd_wasm_shims_runtimes" {
  type    = string
  default = ""
}
variable "containerd_wasm_shims_sha256" {
  type        = string
  description = "JSON-encoded map of wasm shim sha256 checksums. Leave empty to use the default set in locals.pkr.hcl."
  default     = ""
}
variable "containerd_wasm_shims_version" {
  type    = string
  default = "v0.11.1"
}

# --- packer/config/common.json ---
variable "build_target" {
  type    = string
  default = "virt"
}
variable "debug_tools" {
  type    = string
  default = "false"
}
variable "disable_public_repos" {
  type    = string
  default = "false"
}
variable "extra_debs" {
  type    = string
  default = ""
}
variable "extra_kernel_boot_params" {
  type    = string
  default = ""
}
variable "extra_repos" {
  type    = string
  default = ""
}
variable "extra_rpms" {
  type    = string
  default = ""
}
variable "firstboot_custom_roles_post" {
  type    = string
  default = ""
}
variable "firstboot_custom_roles_pre" {
  type    = string
  default = ""
}
variable "http_proxy" {
  type    = string
  default = ""
}
variable "https_proxy" {
  type    = string
  default = ""
}
variable "netplan_removal_excludes" {
  type    = string
  default = ""
}
variable "no_proxy" {
  type    = string
  default = ""
}
variable "node_ansible_tmpdir" {
  type    = string
  default = ""
}
variable "node_custom_roles_post" {
  type        = string
  description = "Comma-separated list of custom ansible roles to run after the standard node role. Set by downstream var-files (e.g. a CI pipeline's own `base` role) to extend provisioning."
  default     = ""
}
variable "node_custom_roles_post_sysprep" {
  type    = string
  default = ""
}
variable "node_custom_roles_pre" {
  type    = string
  default = ""
}
variable "pause_image" {
  type    = string
  default = "registry.k8s.io/pause:3.10.2"
}
variable "pip_conf_file" {
  type    = string
  default = ""
}
variable "python_path" {
  type    = string
  default = ""
}
variable "redhat_epel_rpm" {
  type    = string
  default = "https://dl.fedoraproject.org/pub/epel/epel-release-latest-7.noarch.rpm"
}
variable "epel_rpm_gpg_key" {
  type    = string
  default = ""
}
variable "reenable_public_repos" {
  type    = string
  default = "true"
}
variable "remove_extra_repos" {
  type    = string
  default = "false"
}
variable "ubuntu_repo" {
  type    = string
  default = "http://us.archive.ubuntu.com/ubuntu"
}
variable "ubuntu_security_repo" {
  type    = string
  default = "http://security.ubuntu.com/ubuntu"
}
variable "custom_role_names" {
  type    = string
  default = ""
}
variable "block_nouveau_loading" {
  type    = string
  default = ""
}
variable "amazon_ssm_agent_rpm" {
  type    = string
  default = ""
}
variable "enable_containerd_audit" {
  type    = string
  default = ""
}
variable "kubernetes_enable_automatic_resource_sizing" {
  type    = string
  default = ""
}

# --- packer/config/additional_components.json ---
variable "additional_executables" {
  type    = string
  default = "false"
}
variable "additional_executables_destination_path" {
  type    = string
  default = ""
}
variable "additional_executables_list" {
  type    = string
  default = ""
}
variable "additional_registry_images" {
  type    = string
  default = "false"
}
variable "additional_registry_images_list" {
  type    = string
  default = ""
}
variable "additional_s3" {
  type    = string
  default = "false"
}
variable "additional_url_images" {
  type    = string
  default = "false"
}
variable "additional_url_images_list" {
  type    = string
  default = ""
}
variable "load_additional_components" {
  type    = string
  default = "false"
}

# --- packer/config/ecr_credential_provider.json ---
variable "ecr_credential_provider" {
  type    = string
  default = "false"
}

# --- packer/config/goss-args.json ---
variable "goss_arch" {
  type    = string
  default = "amd64"
}
variable "goss_entry_file" {
  type    = string
  default = "goss/goss.yaml"
}
variable "goss_format" {
  type    = string
  default = "json"
}
variable "goss_format_options" {
  type    = string
  default = "pretty"
}
variable "goss_inspect_mode" {
  type    = string
  default = "false"
}
variable "goss_tests_dir" {
  type    = string
  default = "packer/goss"
}
variable "goss_url" {
  type    = string
  default = ""
}
variable "goss_vars_file" {
  type    = string
  default = "packer/goss/goss-vars.yaml"
}
variable "goss_version" {
  type    = string
  default = "0.3.23"
}

# --- packer/config/ansible-args.json (literal value only; the composed
# ansible_common_vars string and ansible_scp_extra_args live in
# locals.pkr.hcl -- see the note there for why) ---
variable "ansible_common_ssh_args" {
  type    = string
  default = "-o IdentitiesOnly=yes"
}

# Named differently from the legacy "ansible_scp_extra_args" var-file key so
# it can't be clobbered by that key's raw, never-interpolated-by-HCL2 value
# (packer/config/ansible-args.json sets it to the literal string
# "{{env `ANSIBLE_SCP_EXTRA_ARGS`}}"). `env()` is only valid in a variable
# default, not in a locals block, hence this indirection.
variable "ansible_scp_extra_args_env" {
  type    = string
  default = env("ANSIBLE_SCP_EXTRA_ARGS")
}

# --- boilerplate duplicated verbatim in every legacy template's own
# "variables" block (env-sourced, identical everywhere) ---
variable "existing_ansible_ssh_args" {
  type    = string
  default = env("ANSIBLE_SSH_ARGS")
}
variable "ib_version" {
  type    = string
  default = env("IB_VERSION")
}
variable "ansible_user_vars" {
  type    = string
  default = ""
}

# --- FIPS 140 ---------------------------------------------------------------
# With fips_enabled the build VM is switched to FIPS mode (kernel fips=1, FIPS
# crypto policy) and rebooted before provisioning, and Kubernetes, crictl,
# containerd and runc are installed from FIPS builds instead of the upstream
# releases. The defaults point at the Nutanix/D2iQ FIPS builds that NKP's own
# FIPS clusters use (BoringCrypto, FIPS 140 validated). EL9 only.
variable "fips_enabled" {
  type        = bool
  description = "Build a FIPS 140 node image (EL9 only)."
  default     = false
}
variable "fips_kubernetes_rpm_repo" {
  type        = string
  description = "Yum repo with FIPS kubelet/kubeadm/kubectl/cri-tools/kubernetes-cni. Empty uses the Nutanix/D2iQ repo for kubernetes_semver."
  default     = ""
}
variable "fips_kubernetes_rpm_gpg_key" {
  type        = string
  description = "GPG key that signs fips_kubernetes_rpm_repo."
  default     = "https://packages.d2iq.com/konvoy/stable/linux/repos/d2iq-sign-authority-gpg-public-key"
}
variable "fips_kubernetes_build_metadata" {
  type        = string
  description = "Semver build metadata of the FIPS Kubernetes release (v1.36.1+fips.0); image tags use it as v1.36.1_fips.0."
  default     = "fips.0"
}
variable "fips_kubernetes_container_registry" {
  type        = string
  description = "Registry with the FIPS kube-apiserver/controller-manager/scheduler/proxy, etcd and pause images."
  default     = "docker.io/mesosphere"
}
variable "fips_coredns_image_repository" {
  type        = string
  description = "CoreDNS is not rebuilt for FIPS; pre-pull it from here."
  default     = "registry.k8s.io/coredns"
}
variable "fips_etcd_image_tag" {
  type        = string
  description = "FIPS etcd image tag to pre-pull. Empty uses kubeadm's default etcd version for this release with the build metadata appended (e.g. v3.6.8_fips.0)."
  default     = ""
}
variable "fips_containerd_version" {
  type        = string
  description = "Version of the FIPS containerd bundle (used for goss checks and the default URL)."
  default     = "2.3.1"
}
variable "fips_containerd_url" {
  type        = string
  description = "FIPS containerd + runc bundle laid out relative to / (usr/local/bin, usr/local/sbin, etc/systemd/system). Empty uses the Nutanix/D2iQ EL9 bundle for fips_containerd_version."
  default     = ""
}
variable "fips_containerd_sha256" {
  type        = string
  description = "sha256 of fips_containerd_url. Required unless fips_containerd_url is your own mirror you trust without a checksum."
  default     = "03fa9bbcb1bd4b509317c26c3a93e2f909d50ca36fd61e3c616ba5a553473020"
}

# --- DISA STIG ----------------------------------------------------------------
# With stig_enabled the node is hardened with the SCAP Security Guide's DISA
# STIG profile, tailored for Kubernetes (see ansible/roles/stig), scanned, and
# the results kept in the image under /usr/share/image-builder/stig. Implies
# fips_enabled (FIPS mode is a STIG requirement). EL9 only.
variable "stig_enabled" {
  type        = bool
  description = "Harden the image to the DISA STIG (RHEL 9 / Rocky Linux 9). Implies fips_enabled."
  default     = false
}
variable "stig_extra_exceptions" {
  type        = string
  description = "JSON list of extra rules to deselect: [{\"rule\": \"<ssg rule id>\", \"vuln_id\": \"V-...\", \"reason\": \"...\"}]."
  default     = "[]"
}
variable "stig_remote_syslog" {
  type        = string
  description = "host:port of the remote log host for V-258149 (TCP). Empty leaves it an open finding."
  default     = ""
}
variable "stig_grub_password_hash" {
  type        = string
  description = "grub2-mkpasswd-pbkdf2 hash for the GRUB superuser (V-257787). Defaults to $STIG_GRUB_PASSWORD_HASH."
  default     = env("STIG_GRUB_PASSWORD_HASH")
  sensitive   = true
}

variable "stig_partition_sizes" {
  type        = string
  description = "Sizes of the separate STIG filesystems created in the build disk (/var gets the rest, at least 10 GiB). kubelet keeps container logs in /var/log."
  default     = "var_log=8G var_log_audit=4G var_tmp=2G home=1G tmp=2G"
}
