package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	labelManagedBy  = "app.kubernetes.io/managed-by"
	managedByValue  = "nkp-image-builder-webui"
	labelBuildID    = "image-builder.nutanix.com/build-id"
	labelOS         = "image-builder.nutanix.com/os"
	annotationReq   = "image-builder.nutanix.com/request"
	annotationImage = "image-builder.nutanix.com/image-name"
	varsFileKey     = "overrides.json"
	varsMountPath   = "/etc/image-builder"
)

// BuildRequest is what the UI submits. Secret fields are never persisted
// anywhere except the per-build Secret, and are stripped before the request is
// stored as an annotation on the Job.
type BuildRequest struct {
	OS                string `json:"os"`
	KubernetesVersion string `json:"kubernetesVersion"`
	ImageName         string `json:"imageName"`
	ClusterName       string `json:"clusterName"`
	SubnetName        string `json:"subnetName"`
	CPUs              int    `json:"cpus"`
	MemoryMB          int    `json:"memoryMB"`
	DiskSizeGB        int    `json:"diskSizeGB"`
	SourceImageURL    string `json:"sourceImageURL,omitempty"`
	// SourceImage references an image already in Prism Central, by name or
	// UUID, to boot the build VM from instead of downloading SourceImageURL.
	SourceImage string            `json:"sourceImage,omitempty"`
	HTTPProxy   string            `json:"httpProxy,omitempty"`
	HTTPSProxy  string            `json:"httpsProxy,omitempty"`
	NoProxy     string            `json:"noProxy,omitempty"`
	ExtraDebs   string            `json:"extraDebs,omitempty"`
	ExtraRPMs   string            `json:"extraRpms,omitempty"`
	ExtraVars   map[string]string `json:"extraVars,omitempty"`
	// FIPS builds a FIPS 140 image: FIPS kernel mode and crypto policy plus
	// FIPS builds of Kubernetes, containerd and runc.
	FIPS bool `json:"fips,omitempty"`

	// Optional per-build credentials.
	Credentials *BuildCredentials `json:"credentials,omitempty"`
}

type BuildCredentials struct {
	Endpoint string `json:"endpoint,omitempty"`
	Port     string `json:"port,omitempty"`
	Insecure *bool  `json:"insecure,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	RHSMUser string `json:"rhsmUser,omitempty"`
	RHSMPass string `json:"rhsmPass,omitempty"`
}

func (c *BuildCredentials) empty() bool {
	return c == nil || (c.Endpoint == "" && c.Port == "" && c.Insecure == nil &&
		c.Username == "" && c.Password == "" && c.RHSMUser == "" && c.RHSMPass == "")
}

var (
	reK8sVersion = regexp.MustCompile(`^v1\.(\d+)\.(\d+)$`)
	// Prism image names are fairly permissive, but this value is also used in
	// a VM name and passed through make/packer, so keep it conservative.
	reImageName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)
	reNutanixRef = regexp.MustCompile(`^[^\x00-\x1f"'\\` + "`" + `$]{1,128}$`)
	rePackages   = regexp.MustCompile(`^[A-Za-z0-9._+:=~ -]*$`)
	reVarName    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	reProxy      = regexp.MustCompile(`^[^\s"'\\` + "`" + `$]*$`)
	rePort       = regexp.MustCompile(`^[0-9]{1,5}$`)
	reUUID       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reHost       = regexp.MustCompile(`^[A-Za-z0-9.:\-\[\]]{1,253}$`)
)

// Variables the UI controls itself, or that would let a user subvert the
// build (credentials, the build VM's SSH user, arbitrary post-processor
// commands, ansible role injection). Extra vars may not set these.
var deniedExtraVars = map[string]bool{
	"nutanix_endpoint": true, "nutanix_port": true, "nutanix_insecure": true,
	"nutanix_username": true, "nutanix_password": true,
	"nutanix_cluster_name": true, "nutanix_subnet_name": true,
	"nutanix_port_env": true, "nutanix_insecure_env": true,
	"ssh_username": true, "ssh_password": true, "encrypted_ssh_password": true,
	"ssh_private_key_file": true, "ssh_public_key": true,
	"user_data": true, "image_name": true, "build_name": true,
	"custom_post_processor": true, "custom_post_processor_command": true,
	"ansible_extra_vars": true, "ansible_common_vars": true, "ansible_user_vars": true,
	"custom_role_names": true, "node_custom_roles_pre": true, "node_custom_roles_post": true,
	"node_custom_roles_post_sysprep": true, "firstboot_custom_roles_pre": true,
	"firstboot_custom_roles_post": true, "pip_conf_file": true,
	"kubernetes_semver": true, "kubernetes_series": true,
	"kubernetes_deb_version": true, "kubernetes_rpm_version": true,
	"cpus": true, "memory": true, "disk_size_gb": true, "image_url": true,
	"source_image_name": true, "source_image_uuid": true,
	// Would delete the source image from Prism Central after the build --
	// destructive when the source is a shared, pre-existing image.
	"source_image_delete": true,
	// FIPS is a form option and its sources are set by the administrator in
	// the chart (fips.*), so users can't swap in non-FIPS packages. (In FIPS
	// mode the template ignores containerd_version/kubernetes_container_registry
	// in favour of the fips_* sources.)
	"fips_enabled": true,
}

// deniedExtraVarPrefixes extends deniedExtraVars to whole families.
var deniedExtraVarPrefixes = []string{"fips_"}

// buildWrapper runs `make <target>` in its own process group and turns the
// pod's SIGTERM (cancel / delete) into a SIGINT for the whole group -- the same
// thing Ctrl-C does -- so Packer tears down the build VM in Prism Central
// instead of being killed outright. Running make directly as PID 1 would
// leave the signal unhandled and orphan the VM.
const buildWrapper = `set -m
make "$@" &
pid=$!
set +m
trap 'echo "[webui] cancellation requested: interrupting packer so it can clean up the build VM"; kill -INT -- -"$pid" 2>/dev/null' TERM INT
while :; do
  wait "$pid"; rc=$?
  kill -0 "$pid" 2>/dev/null || exit "$rc"
done`

type fieldErrors map[string]string

func (f fieldErrors) Error() string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+f[k])
	}
	return strings.Join(parts, "; ")
}

const defaultImageNameTemplate = "{prefix}{os}{fips}-release-{version}-{timestamp}.qcow2"

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// reYearMonthOS turns "ubuntu-2404" into "ubuntu-24.04" for image names.
var reYearMonthOS = regexp.MustCompile(`^(.+-)(\d{2})(\d{2})$`)

// now is replaced in tests.
var now = time.Now

func defaultImageName(d BuildDefaults, o OSOption, k8sVersion string, fips bool, t time.Time) string {
	tmpl := d.ImageNameTemplate
	if tmpl == "" {
		tmpl = defaultImageNameTemplate
	}
	osName := o.ImageNameOS
	if osName == "" {
		osName = reYearMonthOS.ReplaceAllString(o.ID, "$1$2.$3")
	}
	return strings.NewReplacer(
		"{prefix}", d.ImageNamePrefix,
		"{os}", osName,
		"{version}", strings.TrimPrefix(k8sVersion, "v"),
		"{timestamp}", t.UTC().Format("20060102150405"),
		"{fips}", map[bool]string{true: "-fips"}[fips],
	).Replace(tmpl)
}

// normalize fills defaults and validates. It returns fieldErrors so the UI
// can highlight individual inputs.
func (r *BuildRequest) normalize(cfg *Config) error {
	errs := fieldErrors{}
	d := cfg.Defaults
	trim := func(s *string) { *s = strings.TrimSpace(*s) }
	for _, s := range []*string{&r.OS, &r.KubernetesVersion, &r.ImageName, &r.ClusterName,
		&r.SubnetName, &r.SourceImageURL, &r.SourceImage, &r.HTTPProxy, &r.HTTPSProxy, &r.NoProxy,
		&r.ExtraDebs, &r.ExtraRPMs} {
		trim(s)
	}
	if r.KubernetesVersion == "" {
		r.KubernetesVersion = d.KubernetesVersion
	}
	if r.ClusterName == "" {
		r.ClusterName = d.ClusterName
	}
	if r.SubnetName == "" {
		r.SubnetName = d.SubnetName
	}
	if r.CPUs == 0 {
		r.CPUs = d.CPUs
	}
	if r.MemoryMB == 0 {
		r.MemoryMB = d.MemoryMB
	}
	if r.DiskSizeGB == 0 {
		r.DiskSizeGB = d.DiskSizeGB
	}
	if r.HTTPProxy == "" {
		r.HTTPProxy = d.HTTPProxy
	}
	if r.HTTPSProxy == "" {
		r.HTTPSProxy = d.HTTPSProxy
	}
	if r.NoProxy == "" {
		r.NoProxy = d.NoProxy
	}

	osOpt, ok := cfg.osByID(r.OS)
	if !ok {
		errs["os"] = "unsupported operating system"
	}
	if !reK8sVersion.MatchString(r.KubernetesVersion) {
		errs["kubernetesVersion"] = "must look like v1.36.1"
	}
	if r.ImageName == "" && ok && reK8sVersion.MatchString(r.KubernetesVersion) {
		r.ImageName = defaultImageName(d, osOpt, r.KubernetesVersion, r.FIPS, now())
	}
	if !reImageName.MatchString(r.ImageName) {
		errs["imageName"] = "1-80 chars: letters, digits, '.', '_' or '-'"
	}
	if !reNutanixRef.MatchString(r.ClusterName) {
		errs["clusterName"] = "required"
	}
	if !reNutanixRef.MatchString(r.SubnetName) {
		errs["subnetName"] = "required"
	}
	if r.CPUs < 1 || r.CPUs > 64 {
		errs["cpus"] = "must be between 1 and 64"
	}
	if r.MemoryMB < 1024 || r.MemoryMB > 262144 {
		errs["memoryMB"] = "must be between 1024 and 262144"
	}
	if r.DiskSizeGB < 10 || r.DiskSizeGB > 2048 {
		errs["diskSizeGB"] = "must be between 10 and 2048"
	}
	if r.SourceImageURL != "" {
		u, err := url.Parse(r.SourceImageURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			!reProxy.MatchString(r.SourceImageURL) {
			errs["sourceImageURL"] = "must be an http(s) URL"
		}
	}
	if r.SourceImage != "" {
		if r.SourceImageURL != "" {
			errs["sourceImage"] = "choose either a URL or an existing Prism Central image, not both"
		} else if !reNutanixRef.MatchString(r.SourceImage) {
			errs["sourceImage"] = "invalid image name"
		}
	}
	if r.FIPS {
		switch {
		case !cfg.FIPS.Enabled:
			errs["fips"] = "FIPS builds are not enabled on this service"
		case ok && !osOpt.FIPSCapable:
			errs["fips"] = osOpt.Label + " does not support FIPS builds here; use RHEL 9 or Rocky Linux 9"
		}
	}
	if r.SourceImageURL == "" && r.SourceImage == "" && ok && osOpt.RequiresImageURL {
		errs["sourceImage"] = osOpt.Label + " has no default source image: give a URL or an existing Prism Central image"
	}
	for name, v := range map[string]string{"httpProxy": r.HTTPProxy, "httpsProxy": r.HTTPSProxy, "noProxy": r.NoProxy} {
		if len(v) > 1024 || !reProxy.MatchString(v) {
			errs[name] = "contains invalid characters"
		}
	}
	if len(r.ExtraDebs) > 2048 || !rePackages.MatchString(r.ExtraDebs) {
		errs["extraDebs"] = "space-separated package names only"
	}
	if len(r.ExtraRPMs) > 2048 || !rePackages.MatchString(r.ExtraRPMs) {
		errs["extraRpms"] = "space-separated package names only"
	}
	if len(r.ExtraVars) > 0 {
		if !cfg.Features.AllowExtraVars {
			errs["extraVars"] = "extra Packer variables are disabled on this server"
		} else {
			for k, v := range r.ExtraVars {
				switch {
				case !reVarName.MatchString(k):
					errs["extraVars"] = fmt.Sprintf("invalid variable name %q", k)
				case deniedExtraVars[k] || hasAnyPrefix(k, deniedExtraVarPrefixes):
					errs["extraVars"] = fmt.Sprintf("%q is managed by the form and cannot be overridden", k)
				case len(v) > 4096 || strings.ContainsAny(v, "\x00\n\r"):
					errs["extraVars"] = fmt.Sprintf("value for %q is invalid", k)
				}
			}
		}
	}

	c := r.Credentials
	if !c.empty() {
		if !cfg.Features.AllowCredentialOverride {
			errs["credentials"] = "per-build credentials are disabled on this server"
		} else {
			trim(&c.Endpoint)
			trim(&c.Port)
			trim(&c.Username)
			trim(&c.RHSMUser)
			if c.Endpoint != "" && !reHost.MatchString(c.Endpoint) {
				errs["credentials.endpoint"] = "hostname or IP only (no scheme)"
			}
			if c.Port != "" && !rePort.MatchString(c.Port) {
				errs["credentials.port"] = "must be a port number"
			}
			if (c.Username == "") != (c.Password == "") {
				errs["credentials.password"] = "username and password must be provided together"
			}
			if (c.RHSMUser == "") != (c.RHSMPass == "") {
				errs["credentials.rhsmPass"] = "RHSM username and password must be provided together"
			}
		}
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// packerVars renders the var-file handed to the build via PACKER_VAR_FILES.
// The Makefile places PACKER_VAR_FILES last on the packer command line, so
// these values win over packer/config/*.json and packer/nutanix/<os>.json.
func (r *BuildRequest) packerVars(cfg *Config) map[string]string {
	m := reK8sVersion.FindStringSubmatch(r.KubernetesVersion)
	minor := m[1]
	plain := strings.TrimPrefix(r.KubernetesVersion, "v")

	vars := map[string]string{}
	for k, v := range r.ExtraVars {
		vars[k] = v
	}
	set := func(k, v string) {
		if v != "" {
			vars[k] = v
		}
	}
	set("kubernetes_semver", r.KubernetesVersion)
	set("kubernetes_series", "v1."+minor)
	set("kubernetes_deb_version", plain+"-1.1")
	set("kubernetes_rpm_version", plain)
	// crictl is versioned in lock-step with Kubernetes minors; only default
	// it when the user hasn't pinned one via extra vars.
	if _, pinned := r.ExtraVars["crictl_version"]; !pinned {
		set("crictl_version", "1."+minor+".0")
	}
	set("image_name", r.ImageName)
	set("nutanix_cluster_name", r.ClusterName)
	set("nutanix_subnet_name", r.SubnetName)
	set("cpus", fmt.Sprint(r.CPUs))
	set("memory", fmt.Sprint(r.MemoryMB))
	set("disk_size_gb", fmt.Sprint(r.DiskSizeGB))
	set("image_url", r.SourceImageURL)
	if reUUID.MatchString(r.SourceImage) {
		set("source_image_uuid", strings.ToLower(r.SourceImage))
	} else {
		set("source_image_name", r.SourceImage)
	}
	set("http_proxy", r.HTTPProxy)
	set("https_proxy", r.HTTPSProxy)
	set("no_proxy", r.NoProxy)
	set("extra_debs", r.ExtraDebs)
	set("extra_rpms", r.ExtraRPMs)
	if r.FIPS {
		for k, v := range cfg.FIPS.packerVars(r.KubernetesVersion) {
			vars[k] = v
		}
		vars["fips_enabled"] = "true"
	}
	return vars
}

// redacted returns a copy safe to store on the Job and show in the UI.
func (r BuildRequest) redacted() BuildRequest {
	if !r.Credentials.empty() {
		c := *r.Credentials
		if c.Password != "" {
			c.Password = "********"
		}
		if c.RHSMPass != "" {
			c.RHSMPass = "********"
		}
		r.Credentials = &c
	} else {
		r.Credentials = nil
	}
	return r
}

func (r *BuildRequest) credentialEnv() map[string]string {
	env := map[string]string{}
	c := r.Credentials
	if c.empty() {
		return env
	}
	add := func(k, v string) {
		if v != "" {
			env[k] = v
		}
	}
	add("NUTANIX_ENDPOINT", c.Endpoint)
	add("NUTANIX_PORT", c.Port)
	if c.Insecure != nil {
		env["NUTANIX_INSECURE"] = fmt.Sprint(*c.Insecure)
	}
	add("NUTANIX_USERNAME", c.Username)
	add("NUTANIX_PASSWORD", c.Password)
	add("RHSM_USER", c.RHSMUser)
	add("RHSM_PASS", c.RHSMPass)
	return env
}

func newBuildID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// buildObjects assembles the ConfigMap, optional Secret and Job for a build.
// Owner references from the ConfigMap/Secret to the Job are added after the
// Job is created (its UID isn't known yet), so the dependents are garbage
// collected together with the Job.
func buildObjects(cfg *Config, req *BuildRequest, id string) (*corev1.ConfigMap, *corev1.Secret, *batchv1.Job, error) {
	name := "nkp-ib-" + id
	labels := map[string]string{
		labelManagedBy: managedByValue,
		labelBuildID:   id,
		labelOS:        req.OS,
	}

	varsJSON, err := json.MarshalIndent(req.packerVars(cfg), "", "  ")
	if err != nil {
		return nil, nil, nil, err
	}
	reqJSON, err := json.Marshal(req.redacted())
	if err != nil {
		return nil, nil, nil, err
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.Namespace, Labels: labels},
		Data:       map[string]string{varsFileKey: string(varsJSON)},
	}

	envFrom := []corev1.EnvFromSource{{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: cfg.Nutanix.CredentialsSecret},
		},
	}}

	var secret *corev1.Secret
	if credEnv := req.credentialEnv(); len(credEnv) > 0 {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-creds", Namespace: cfg.Namespace, Labels: labels},
			Type:       corev1.SecretTypeOpaque,
			StringData: credEnv,
		}
		// Later envFrom sources win, so per-build values override the defaults.
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name},
			},
		})
	}

	env := []corev1.EnvVar{
		{Name: "PACKER_VAR_FILES", Value: varsMountPath + "/" + varsFileKey},
		{Name: "PACKER_COLOR", Value: "false"},
		{Name: "PACKER_NO_COLOR", Value: "1"},
		{Name: "ANSIBLE_NOCOLOR", Value: "1"},
		{Name: "ANSIBLE_FORCE_COLOR", Value: "0"},
		{Name: "CHECKPOINT_DISABLE", Value: "1"},
	}
	env = append(env, cfg.Builder.ExtraEnv...)

	var pullSecrets []corev1.LocalObjectReference
	for _, s := range cfg.Builder.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: s})
	}

	backoff := int32(0)     // a failed packer build should not silently retry
	automountToken := false // builds never talk to the Kubernetes API

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				annotationReq:   string(reqJSON),
				annotationImage: req.ImageName,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: cfg.Builder.TTLSecondsAfterFinished,
			ActiveDeadlineSeconds:   cfg.Builder.ActiveDeadlineSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					ServiceAccountName:            cfg.Builder.ServiceAccountName,
					AutomountServiceAccountToken:  &automountToken,
					ImagePullSecrets:              pullSecrets,
					NodeSelector:                  cfg.Builder.NodeSelector,
					Tolerations:                   cfg.Builder.Tolerations,
					TerminationGracePeriodSeconds: cfg.Builder.TerminationGracePeriodSeconds,
					SecurityContext:               cfg.Builder.PodSecurityContext,
					Containers: []corev1.Container{{
						Name:            "image-builder",
						Image:           cfg.Builder.Image,
						ImagePullPolicy: cfg.Builder.ImagePullPolicy,
						Command:         []string{"/bin/bash", "-c", buildWrapper, "build-wrapper"},
						Args:            []string{"build-nutanix-" + req.OS},
						Env:             env,
						EnvFrom:         envFrom,
						Resources:       cfg.Builder.Resources,
						SecurityContext: cfg.Builder.SecurityContext,
						VolumeMounts: []corev1.VolumeMount{{
							Name: "build-vars", MountPath: varsMountPath, ReadOnly: true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "build-vars",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name},
							},
						},
					}},
				},
			},
		},
	}
	return cm, secret, job, nil
}
