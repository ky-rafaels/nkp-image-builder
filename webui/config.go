package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

var reDriverBranch = regexp.MustCompile(`^[0-9]{3,4}$`)

// Config is loaded from a JSON file rendered by the Helm chart (see
// charts/nkp-image-builder/templates/configmap.yaml). Everything a viewer of
// the UI may see lives under Catalog/Defaults; everything about how build Jobs
// are shaped lives under Builder.
type Config struct {
	// Namespace build Jobs are created in. Defaults to the pod's namespace.
	Namespace string `json:"namespace"`

	Builder  BuilderConfig `json:"builder"`
	Nutanix  NutanixConfig `json:"nutanix"`
	Defaults BuildDefaults `json:"defaults"`
	Catalog  CatalogConfig `json:"catalog"`
	Features FeatureConfig `json:"features"`
	FIPS     FIPSConfig    `json:"fips"`
	STIG     STIGConfig    `json:"stig"`
	GPU      GPUConfig     `json:"gpu"`
}

// GPUConfig controls the "Attach a GPU" build option: a GPU or vGPU profile
// from the build cluster is attached to the build VM, and the NVIDIA driver
// for it is installed and checked with nvidia-smi (see
// images/capi/ansible/roles/gpu).
type GPUConfig struct {
	Enabled bool `json:"enabled"`
	// Discovery lists the build cluster's GPUs from Prism Central in the form
	// and checks the chosen one before a build starts. The UI pod then needs
	// the Prism Central credentials (NUTANIX_* env, set by the chart).
	Discovery bool         `json:"discovery"`
	NVIDIA    NVIDIAConfig `json:"nvidia"`
}

// NVIDIAConfig sets the gpu_nvidia_* Packer variables. Empty fields keep the
// defaults in packer/nutanix/variables.pkr.hcl.
type NVIDIAConfig struct {
	// Data center driver branch for passthrough GPUs, e.g. "580".
	DriverBranch string `json:"driverBranch"`
	// Open kernel modules (Turing and newer). False for P4/P40/P100/V100.
	OpenKernelModules *bool `json:"openKernelModules"`
	// RHEL/Rocky: mirror of NVIDIA's CUDA repo and its signing key.
	RPMRepo   string `json:"rpmRepo"`
	RPMGPGKey string `json:"rpmGpgKey"`
	// NVIDIA vGPU guest driver from the NVIDIA Licensing Portal, on a mirror.
	// vGPU profiles can only be picked for an OS that has one.
	VGPUDriver VGPUDrivers `json:"vgpuDriver"`
}

// VGPUDrivers are the vGPU guest driver's packages. The .run works on every
// OS; the .rpm (RHEL/Rocky) and .deb (Ubuntu) are used instead where set.
// DISA STIG builds need the .rpm: fapolicyd only trusts packaged files.
type VGPUDrivers struct {
	Run VGPUDriver `json:"run"`
	RPM VGPUDriver `json:"rpm"`
	DEB VGPUDriver `json:"deb"`
}

type VGPUDriver struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// defaultNVIDIADriverBranch matches gpu_nvidia_driver_branch in
// packer/nutanix/variables.pkr.hcl; it is only shown in the UI.
const defaultNVIDIADriverBranch = "580"

func (n NVIDIAConfig) Branch() string {
	if n.DriverBranch != "" {
		return n.DriverBranch
	}
	return defaultNVIDIADriverBranch
}

// packageFamily is "rpm" or "deb" for the OSes the GPU build supports.
func packageFamily(osID string) string {
	switch {
	case strings.HasPrefix(osID, "ubuntu"):
		return "deb"
	case strings.HasPrefix(osID, "rhel"), strings.HasPrefix(osID, "rocky"),
		strings.HasPrefix(osID, "alma"), strings.HasPrefix(osID, "centos"):
		return "rpm"
	}
	return ""
}

// vgpuDriverFor picks the vGPU guest driver package for an OS.
func (n NVIDIAConfig) vgpuDriverFor(osID string, stig bool) (VGPUDriver, bool) {
	d := n.VGPUDriver
	switch packageFamily(osID) {
	case "rpm":
		if d.RPM.URL != "" {
			return d.RPM, true
		}
	case "deb":
		if d.DEB.URL != "" {
			return d.DEB, true
		}
	}
	if !stig && d.Run.URL != "" {
		return d.Run, true
	}
	return VGPUDriver{}, false
}

// packerVars maps the configured driver settings to Packer variables; the
// vGPU guest driver only goes to vGPU builds.
func (n NVIDIAConfig) packerVars(vgpu *VGPUDriver) map[string]string {
	out := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	set("gpu_nvidia_driver_branch", n.DriverBranch)
	if n.OpenKernelModules != nil {
		out["gpu_nvidia_open_kernel_modules"] = fmt.Sprint(*n.OpenKernelModules)
	}
	set("gpu_nvidia_rpm_repo", n.RPMRepo)
	set("gpu_nvidia_rpm_gpg_key", n.RPMGPGKey)
	if vgpu != nil {
		set("gpu_nvidia_vgpu_driver_url", vgpu.URL)
		set("gpu_nvidia_vgpu_driver_sha256", vgpu.SHA256)
	}
	return out
}

// STIGConfig controls the "DISA STIG" build option (see
// images/capi/ansible/roles/stig). A STIG build is always a FIPS build.
type STIGConfig struct {
	Enabled bool `json:"enabled"`
	// Site exceptions added to the built-in Kubernetes ones.
	ExtraExceptions []STIGException `json:"extraExceptions"`
	// host:port of the remote log host (V-258149).
	RemoteSyslog string `json:"remoteSyslog"`
}

type STIGException struct {
	Rule   string `json:"rule"`
	VulnID string `json:"vuln_id,omitempty"`
	Reason string `json:"reason"`
}

// FIPSConfig controls the "FIPS 140" build option. Empty source fields fall
// back to the defaults in packer/nutanix/variables.pkr.hcl (the Nutanix/D2iQ
// FIPS builds NKP itself uses); set them to point at mirrors.
type FIPSConfig struct {
	Enabled bool `json:"enabled"`
	// Preflight checks, before starting a FIPS build, that the RPM repo has
	// packages for the requested Kubernetes version. Network errors don't
	// block the build; only a definite "not found" does.
	Preflight bool `json:"preflight"`
	// Packer variable overrides, all optional. KubernetesRPMRepo may contain
	// {version} (e.g. v1.36.1).
	KubernetesRPMRepo         string `json:"kubernetesRpmRepo"`
	KubernetesRPMGPGKey       string `json:"kubernetesRpmGpgKey"`
	KubernetesBuildMetadata   string `json:"kubernetesBuildMetadata"`
	KubernetesImageRepository string `json:"kubernetesImageRepository"`
	CoreDNSImageRepository    string `json:"corednsImageRepository"`
	EtcdImageTag              string `json:"etcdImageTag"`
	ContainerdVersion         string `json:"containerdVersion"`
	ContainerdURL             string `json:"containerdURL"`
	ContainerdSHA256          string `json:"containerdSHA256"`
}

// defaultFIPSRPMRepo matches the default in packer/nutanix/locals.pkr.hcl.
const defaultFIPSRPMRepo = "https://packages.d2iq.com/konvoy/stable/linux/repos/el/kubernetes-{version}-fips/x86_64"

// RPMRepo is the FIPS Kubernetes yum repo for a version like v1.36.1.
func (f FIPSConfig) RPMRepo(k8sVersion string) string {
	repo := f.KubernetesRPMRepo
	if repo == "" {
		repo = defaultFIPSRPMRepo
	}
	return strings.TrimRight(strings.ReplaceAll(repo, "{version}", k8sVersion), "/")
}

// packerVars maps the configured sources to Packer variables. The RPM repo is
// always passed so the preflight check and the build agree on it.
func (f FIPSConfig) packerVars(k8sVersion string) map[string]string {
	all := map[string]string{
		"fips_kubernetes_rpm_repo":           f.RPMRepo(k8sVersion),
		"fips_kubernetes_rpm_gpg_key":        f.KubernetesRPMGPGKey,
		"fips_kubernetes_build_metadata":     f.KubernetesBuildMetadata,
		"fips_kubernetes_container_registry": f.KubernetesImageRepository,
		"fips_coredns_image_repository":      f.CoreDNSImageRepository,
		"fips_etcd_image_tag":                f.EtcdImageTag,
		"fips_containerd_version":            f.ContainerdVersion,
		"fips_containerd_url":                f.ContainerdURL,
		"fips_containerd_sha256":             f.ContainerdSHA256,
	}
	out := map[string]string{}
	for k, v := range all {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// BuildMetadata is the semver build metadata of FIPS Kubernetes releases.
func (f FIPSConfig) BuildMetadata() string {
	if f.KubernetesBuildMetadata != "" {
		return f.KubernetesBuildMetadata
	}
	return "fips.0"
}

// ImageRepository is where the FIPS control-plane and etcd images live.
func (f FIPSConfig) ImageRepository() string {
	if f.KubernetesImageRepository != "" {
		return f.KubernetesImageRepository
	}
	return "docker.io/mesosphere"
}

type BuilderConfig struct {
	Image                   string                      `json:"image"`
	ImagePullPolicy         corev1.PullPolicy           `json:"imagePullPolicy"`
	ImagePullSecrets        []string                    `json:"imagePullSecrets"`
	ServiceAccountName      string                      `json:"serviceAccountName"`
	Resources               corev1.ResourceRequirements `json:"resources"`
	NodeSelector            map[string]string           `json:"nodeSelector"`
	Tolerations             []corev1.Toleration         `json:"tolerations"`
	ExtraEnv                []corev1.EnvVar             `json:"extraEnv"`
	TTLSecondsAfterFinished *int32                      `json:"ttlSecondsAfterFinished"`
	ActiveDeadlineSeconds   *int64                      `json:"activeDeadlineSeconds"`
	// Packer needs time to delete the build VM in Prism Central when a build
	// is cancelled, so this is deliberately generous.
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds"`
	// The image-builder image declares `USER imagebuilder` by name, so
	// runAsNonRoot needs an explicit numeric runAsUser to be verifiable.
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext"`
	SecurityContext    *corev1.SecurityContext    `json:"securityContext"`
}

type NutanixConfig struct {
	// Secret holding NUTANIX_ENDPOINT/PORT/INSECURE/USERNAME/PASSWORD (and
	// optionally RHSM_USER/RHSM_PASS). Injected into every build via envFrom.
	CredentialsSecret string `json:"credentialsSecret"`
	// Endpoint is shown in the UI for context only; the Job reads the Secret.
	Endpoint string `json:"endpoint"`
}

type BuildDefaults struct {
	ClusterName       string `json:"clusterName"`
	SubnetName        string `json:"subnetName"`
	KubernetesVersion string `json:"kubernetesVersion"`
	CPUs              int    `json:"cpus"`
	MemoryMB          int    `json:"memoryMB"`
	DiskSizeGB        int    `json:"diskSizeGB"`
	// LargeBuild sizes the build VM for DISA STIG and GPU builds, which
	// compile the NVIDIA driver, rebuild initramfs images and run OpenSCAP
	// and AIDE over the whole disk. The build VM is deleted afterwards, so
	// its size isn't part of the image. Never smaller than CPUs/MemoryMB.
	LargeBuild      VMSize `json:"largeBuild"`
	ImageNamePrefix string `json:"imageNamePrefix"`
	// ImageNameTemplate builds the default image name. Placeholders:
	// {prefix} ImageNamePrefix, {os} OSOption.ImageNameOS (e.g. ubuntu-24.04),
	// {version} Kubernetes version without the "v" (e.g. 1.36.1),
	// {timestamp} UTC build time as YYYYMMDDhhmmss,
	// {fips} "-fips" for FIPS builds, "" otherwise,
	// {stig} "-stig" for DISA STIG builds, "" otherwise.
	ImageNameTemplate string `json:"imageNameTemplate"`
	HTTPProxy         string `json:"httpProxy"`
	HTTPSProxy        string `json:"httpsProxy"`
	NoProxy           string `json:"noProxy"`
}

type OSOption struct {
	ID    string `json:"id"`    // matches packer/nutanix/<id>.json and build-nutanix-<id>
	Label string `json:"label"` // human readable
	// DefaultImageURL is informational. When RequiresImageURL is set the
	// user must supply a source image -- either a URL or an image already in
	// Prism Central (e.g. a RHEL KVM guest image).
	DefaultImageURL  string `json:"defaultImageURL"`
	RequiresImageURL bool   `json:"requiresImageURL"`
	RequiresRHSM     bool   `json:"requiresRHSM"`
	// FIPSCapable offers the FIPS 140 option for this OS. Only EL9 (RHEL 9,
	// Rocky 9) is wired up in the build pipeline.
	FIPSCapable bool `json:"fipsCapable"`
	// STIGCapable offers DISA STIG hardening (needs FIPSCapable too).
	STIGCapable bool `json:"stigCapable"`
	// GPUCapable offers attaching a GPU and installing the NVIDIA driver.
	// The build pipeline supports Ubuntu, RHEL 9 and Rocky Linux 9.
	GPUCapable bool `json:"gpuCapable"`
	// ImageNameOS is the {os} part of default image names. Derived from ID
	// when empty: ubuntu-2404 -> ubuntu-24.04, rockylinux-9 -> rockylinux-9.
	ImageNameOS string `json:"imageNameOS"`
}

type CatalogConfig struct {
	OperatingSystems   []OSOption `json:"operatingSystems"`
	KubernetesVersions []string   `json:"kubernetesVersions"`
}

type FeatureConfig struct {
	// Let users enter their own Prism Central credentials per build instead
	// of always using the chart-provided Secret.
	AllowCredentialOverride bool `json:"allowCredentialOverride"`
	// Let users pass arbitrary extra Packer variables (subject to a deny-list).
	AllowExtraVars bool `json:"allowExtraVars"`
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config %s: %w", path, err)
		}
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parsing config %s: %w", path, err)
		}
	}
	cfg.applyDefaults()
	if cfg.Builder.Image == "" {
		return nil, fmt.Errorf("builder.image must be set")
	}
	if cfg.Nutanix.CredentialsSecret == "" {
		return nil, fmt.Errorf("nutanix.credentialsSecret must be set")
	}
	if len(cfg.Catalog.OperatingSystems) == 0 {
		return nil, fmt.Errorf("catalog.operatingSystems must not be empty")
	}
	if b := cfg.GPU.NVIDIA.DriverBranch; b != "" && !reDriverBranch.MatchString(b) {
		return nil, fmt.Errorf("gpu.nvidia.driverBranch must be a driver branch number like 580, not %q", b)
	}
	d := cfg.GPU.NVIDIA.VGPUDriver
	for _, c := range []struct {
		kind string
		drv  VGPUDriver
	}{{"run", d.Run}, {"rpm", d.RPM}, {"deb", d.DEB}} {
		if c.drv.URL == "" {
			continue
		}
		p, err := url.Parse(c.drv.URL)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" ||
			!strings.HasSuffix(strings.ToLower(p.Path), "."+c.kind) {
			return nil, fmt.Errorf("gpu.nvidia.vgpuDriver.%s.url must be an http(s) URL of a .%s file", c.kind, c.kind)
		}
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Namespace == "" {
		c.Namespace = os.Getenv("POD_NAMESPACE")
	}
	if c.Namespace == "" {
		c.Namespace = "default"
	}
	if c.Builder.ImagePullPolicy == "" {
		c.Builder.ImagePullPolicy = corev1.PullIfNotPresent
	}
	if c.Builder.TerminationGracePeriodSeconds == nil {
		v := int64(600)
		c.Builder.TerminationGracePeriodSeconds = &v
	}
	if c.Builder.PodSecurityContext == nil {
		uid, nonRoot := int64(1001), true
		c.Builder.PodSecurityContext = &corev1.PodSecurityContext{
			RunAsUser:      &uid,
			RunAsGroup:     &uid,
			RunAsNonRoot:   &nonRoot,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
	}
	if c.Builder.SecurityContext == nil {
		noEscalation := false
		c.Builder.SecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: &noEscalation,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	}
	for i := range c.Catalog.OperatingSystems {
		o := &c.Catalog.OperatingSystems[i]
		if o.ImageNameOS == "" {
			o.ImageNameOS = reYearMonthOS.ReplaceAllString(o.ID, "$1$2.$3")
		}
	}
	d := &c.Defaults
	if d.ImageNameTemplate == "" {
		d.ImageNameTemplate = defaultImageNameTemplate
	}
	if d.KubernetesVersion == "" {
		d.KubernetesVersion = "v1.36.1"
	}
	if d.CPUs == 0 {
		d.CPUs = 2
	}
	if d.MemoryMB == 0 {
		d.MemoryMB = 4096
	}
	if d.DiskSizeGB == 0 {
		d.DiskSizeGB = 40
	}
	if d.LargeBuild.CPUs == 0 {
		d.LargeBuild.CPUs = defaultLargeBuildCPUs
	}
	if d.LargeBuild.MemoryMB == 0 {
		d.LargeBuild.MemoryMB = defaultLargeBuildMemoryMB
	}
	d.LargeBuild.CPUs = max(d.LargeBuild.CPUs, d.CPUs)
	d.LargeBuild.MemoryMB = max(d.LargeBuild.MemoryMB, d.MemoryMB)
}

type VMSize struct {
	CPUs     int `json:"cpus"`
	MemoryMB int `json:"memoryMB"`
}

const (
	defaultLargeBuildCPUs     = 4
	defaultLargeBuildMemoryMB = 8192
)

// vmSize is the default build VM size for a request.
func (d BuildDefaults) vmSize(large bool) VMSize {
	if large {
		return d.LargeBuild
	}
	return VMSize{CPUs: d.CPUs, MemoryMB: d.MemoryMB}
}

func (c *Config) osByID(id string) (OSOption, bool) {
	for _, o := range c.Catalog.OperatingSystems {
		if o.ID == id {
			return o, true
		}
	}
	return OSOption{}, false
}
