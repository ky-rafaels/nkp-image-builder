build {
  sources = ["source.nutanix.node"]

  post-processor "shell-local" {
    environment_vars = [
      "CUSTOM_POST_PROCESSOR=${var.custom_post_processor}"
    ]
    inline = [
      "if [ \"$CUSTOM_POST_PROCESSOR\" != \"true\" ]; then exit 0; fi",
      var.custom_post_processor_command,
    ]
  }

  provisioner "shell" {
    environment_vars = [
      "BUILD_NAME=${var.build_name}",
      "PYPY_HTTP_SOURCE=${var.pypy_http_source}",
    ]
    execute_command = "BUILD_NAME=${var.build_name}; if [[ \"$${BUILD_NAME}\" == *\"flatcar\"* ]]; then sudo {{.Vars}} -S -E bash '{{.Path}}'; fi"
    script          = "./packer/files/flatcar/scripts/bootstrap-flatcar.sh"
  }

  # FIPS: switch the VM to FIPS mode and reboot into it before provisioning,
  # so everything below (including goss) runs on the FIPS kernel. All three
  # steps are no-ops when fips_enabled is false.
  provisioner "shell" {
    environment_vars = [
      "FIPS_ENABLED=${local.fips_enabled}",
      "http_proxy=${var.http_proxy}",
      "https_proxy=${var.https_proxy}",
      "no_proxy=${var.no_proxy}",
    ]
    execute_command = "sudo {{.Vars}} -S -E bash '{{.Path}}'"
    script          = "./packer/files/fips/enable-fips.sh"
  }

  # DISA STIG: separate /var, /var/log, /var/log/audit, /var/tmp, /home and
  # /tmp filesystems, mounted by the reboot below. No-op unless stig_enabled.
  provisioner "shell" {
    environment_vars = [
      "STIG_ENABLED=${var.stig_enabled}",
      "STIG_PARTITION_SIZES=${var.stig_partition_sizes}",
      "http_proxy=${var.http_proxy}",
      "https_proxy=${var.https_proxy}",
      "no_proxy=${var.no_proxy}",
    ]
    execute_command = "sudo {{.Vars}} -S -E bash '{{.Path}}' create"
    script          = "./packer/files/stig/stig-partitions.sh"
  }

  provisioner "shell" {
    execute_command   = local.fips_enabled ? "sudo systemctl reboot" : "true"
    inline            = ["true"]
    expect_disconnect = true
  }

  provisioner "shell" {
    environment_vars    = ["FIPS_ENABLED=${local.fips_enabled}"]
    execute_command     = "sudo {{.Vars}} -S -E bash '{{.Path}}'"
    pause_before        = local.fips_enabled ? "30s" : "0s"
    start_retry_timeout = "15m"
    script              = "./packer/files/fips/verify-fips.sh"
  }

  provisioner "shell" {
    environment_vars = [
      "STIG_ENABLED=${var.stig_enabled}",
      "BUILD_USER=${var.ssh_username}",
    ]
    execute_command = "sudo {{.Vars}} -S -E bash '{{.Path}}' finish"
    script          = "./packer/files/stig/stig-partitions.sh"
  }

  provisioner "ansible" {
    ansible_env_vars = [
      "ANSIBLE_SSH_ARGS='${var.existing_ansible_ssh_args} -o IdentitiesOnly=yes'"
    ]
    extra_arguments = [
      "--extra-vars", local.ansible_common_vars,
      "--extra-vars", var.ansible_extra_vars,
      "--extra-vars", var.ansible_user_vars,
      # JSON so values with spaces/quotes survive; the grub hash is a
      # sensitive variable, so Packer redacts it from its output.
      "--extra-vars", jsonencode({
        stig_enabled            = var.stig_enabled
        stig_extra_exceptions   = jsondecode(var.stig_extra_exceptions)
        stig_remote_syslog      = var.stig_remote_syslog
        stig_grub_password_hash = var.stig_grub_password_hash
        # goss runs after the STIG role has fapolicyd enforcing.
        stig_fapolicyd_build_allow_paths = [local.goss_download_path]
      }),
      "--scp-extra-args=${local.ansible_scp_extra_args}",
    ]
    playbook_file = "./ansible/node.yml"
    user          = "builder"
  }

  provisioner "goss" {
    arch           = var.goss_arch
    download_path  = local.goss_download_path
    format         = var.goss_format
    format_options = var.goss_format_options
    goss_file      = var.goss_entry_file
    inspect        = var.goss_inspect_mode
    tests          = [var.goss_tests_dir]
    url            = var.goss_url
    use_sudo       = true
    vars_file      = var.goss_vars_file
    vars_inline = {
      ARCH                                   = "amd64"
      OS                                     = lower(var.distro_name)
      OS_VERSION                             = lower(var.distribution_version)
      PROVIDER                               = "nutanix"
      containerd_enable_limit_no_file        = var.containerd_enable_limit_no_file
      containerd_gvisor_runtime              = var.containerd_gvisor_runtime
      containerd_gvisor_version              = var.containerd_gvisor_version
      containerd_image_pull_progress_timeout = (var.containerd_image_pull_progress_timeout != null ? var.containerd_image_pull_progress_timeout : "")
      containerd_version                     = local.containerd_version
      fips_enabled                           = local.fips_enabled
      kubernetes_build_metadata              = local.kubernetes_build_metadata
      kubernetes_cni_deb_version             = (var.kubernetes_cni_deb_version != null ? var.kubernetes_cni_deb_version : "")
      kubernetes_cni_rpm_version             = split("-", var.kubernetes_cni_rpm_version != null ? var.kubernetes_cni_rpm_version : "")[0]
      kubernetes_cni_source_type             = var.kubernetes_cni_source_type
      kubernetes_cni_version                 = replace(var.kubernetes_cni_semver, "v", "")
      kubernetes_deb_version                 = var.kubernetes_deb_version
      kubernetes_rpm_version                 = split("-", var.kubernetes_rpm_version)[0]
      kubernetes_source_type                 = var.kubernetes_source_type
      kubernetes_version                     = replace(var.kubernetes_semver, "v", "")
      pause_image                            = var.pause_image
    }
    version = var.goss_version
  }

  # DISA STIG: deferred rules, runtime checks under fapolicyd, AIDE baseline
  # and the final scan. Runs last, before shutdown_command deletes the build
  # user. This step removes the build user's NOPASSWD sudo and leaves it
  # password sudo only, hence the password on stdin here and in
  # shutdown_command.
  provisioner "shell" {
    environment_vars = [
      "STIG_ENABLED=${var.stig_enabled}",
      "BUILD_USER=${var.ssh_username}",
    ]
    execute_command  = "echo '${var.ssh_password}' | sudo -S {{.Vars}} -E bash '{{.Path}}'"
    script           = "./packer/files/stig/finalize-stig.sh"
  }
}
