package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	// Containerd pins containerd and runc per Kubernetes minor ("v1.35").
	// Entries replace the built-in pair for the same minor.
	Containerd map[string]ContainerdRelease `json:"containerd"`
	// Airgap offers air-gapped builds, which install only from the artifact
	// store, and harvesting builds, which fill it.
	Airgap AirgapConfig `json:"airgap"`
}

// AirgapConfig is the artifact store: a directory on the UI pod's
// PersistentVolume. Build pods download what an air-gapped build needs from
// it into an emptyDir, so the volume needs no ReadWriteMany.
type AirgapConfig struct {
	Enabled bool `json:"enabled"`
	// ArtifactsDir is the store's directory in the UI pod.
	ArtifactsDir string `json:"artifactsDir"`
	// ServiceURL is how build pods reach the UI, e.g.
	// http://nkp-image-builder.nkp-image-builder.svc:80.
	ServiceURL string `json:"serviceURL"`
	// TokenSecret is the Secret whose "token" build pods send to the store.
	// The UI reads the same token from $ARTIFACTS_TOKEN.
	TokenSecret string `json:"tokenSecret"`
	// BuildVolumeSizeLimit caps the build pod's emptyDir for artifacts and
	// harvested packages. Default 40Gi.
	BuildVolumeSizeLimit string `json:"buildVolumeSizeLimit"`
	// MaxUploadBytes caps one uploaded artifact. Default 20 GiB.
	MaxUploadBytes int64 `json:"maxUploadBytes"`
}

// ContainerdRelease is the containerd and runc a build installs for one
// Kubernetes minor. NKP's containerd-<version>-d2iq.1 bundles are built from
// the upstream containerd and runc tags, so an OS without a bundle gets those
// upstream releases; FIPS builds get the BoringCrypto EL9 bundle.
type ContainerdRelease struct {
	Containerd string `json:"containerd"`
	Runc       string `json:"runc"`
	// FIPSSHA256 is the sha256 of
	// containerd-<containerd>-d2iq.1-rhel-9.6-x86_64_fips.tar.gz. FIPS builds
	// only use this pair when it is set.
	FIPSSHA256 string `json:"fipsSha256"`
	// Bundles are NKP's own non-FIPS builds of this containerd, by operating
	// system ID. A build for a listed OS installs its bundle, so containerd
	// reports NKP's version (e.g. 2.1.6-d2iq.1).
	Bundles map[string]ContainerdBundle `json:"bundles"`
}

// ContainerdBundle is one of NKP's containerd + runc bundles. packages.d2iq.com
// publishes no checksums, so the sha256 is pinned here.
type ContainerdBundle struct {
	// Distro names the bundle: containerd-<containerd>-d2iq.1-<distro>-x86_64.tar.gz.
	Distro string `json:"distro"`
	// URL replaces the packages.d2iq.com URL, e.g. with a mirror.
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

const d2iqContainerdBundleURL = "https://packages.d2iq.com/dkp/containerd/containerd-%s-d2iq.1-%s-x86_64.tar.gz"

// BundleURL is where the bundle for containerd version v is downloaded from.
func (b ContainerdBundle) BundleURL(v string) string {
	if b.URL != "" {
		return b.URL
	}
	return fmt.Sprintf(d2iqContainerdBundleURL, v, b.Distro)
}

// defaultContainerdReleases match NKP's own node images: containerd
// 2.1.6-d2iq.1 for Kubernetes 1.35 and 2.3.1-d2iq.1 for 1.36. Minors not
// listed get the Packer defaults (packer/config/containerd.json).
// containerd 2.3 writes config version 4 and refuses a drop-in newer than
// config.toml, which 2.1 does not. Both install NKP's bundles where one
// exists for the OS (no ubuntu-26.04 builds; no rhel-9.8 build of 2.1.6, and
// the EL9 bundles only need glibc 2.34, so any RHEL 9 minor runs them).
var defaultContainerdReleases = map[string]ContainerdRelease{
	"v1.35": {Containerd: "2.1.6", Runc: "1.3.4", FIPSSHA256: "8cf89db611b2a09a3829a054dab0155f04cec32880eb0344825fafa1ed6863e6",
		Bundles: map[string]ContainerdBundle{
			"rockylinux-9": {Distro: "rocky-9.8", SHA256: "540f5fc696649481e3634c8fbd7b3016560118530afede34f720576e90e99114"},
			"rhel-9":       {Distro: "rhel-9.6", SHA256: "d7f1c2e061bf2e619e41bba281cc26545f906d6bc82b49f94d3a09e4954d5496"},
			"ubuntu-2204":  {Distro: "ubuntu-22.04", SHA256: "89e18f2f188f7ae8a4432f3cd5ea76aec2689fd02e60f93338eee14a3a164872"},
			"ubuntu-2404":  {Distro: "ubuntu-24.04", SHA256: "1cfa30026773df76220c605485aac5a2c4f88660ffe723c4bd3a658607b846bd"},
		}},
	"v1.36": {Containerd: "2.3.1", Runc: "1.4.2", FIPSSHA256: "03fa9bbcb1bd4b509317c26c3a93e2f909d50ca36fd61e3c616ba5a553473020",
		Bundles: map[string]ContainerdBundle{
			"rockylinux-9": {Distro: "rocky-9.8", SHA256: "212f233a3214f1031ad88dc05f771992d89fb1ea3135b6a8529458cb631ea1d5"},
			"rhel-9":       {Distro: "rhel-9.8", SHA256: "03ff4589cee96342fba90fc1fae49c5d87ba3d5e7ea231ca32844573386e3cf6"},
			"ubuntu-2204":  {Distro: "ubuntu-22.04", SHA256: "590bc3c34e6ee2596c9494104f4a86d0f69d3d00e8f8241a9213c9b17d007d69"},
			"ubuntu-2404":  {Distro: "ubuntu-24.04", SHA256: "405a67a5984a84945194152b24f69064d378ddc5d7475477ede5d1c17cd7ed9c"},
		}},
}

var (
	reK8sMinor = regexp.MustCompile(`^v1\.[0-9]+$`)
	reSemver   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	reSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reDistro   = regexp.MustCompile(`^[a-z]+-[0-9]+(\.[0-9]+)*$`)
)

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
	OperatingSystems []OSOption `json:"operatingSystems"`
	// KubernetesVersions are the versions in the form's dropdown, newest
	// first. Users can still pick "Other version…" and type any v1.X.Y.
	KubernetesVersions []string `json:"kubernetesVersions"`
}

// defaultKubernetesVersions fill the version dropdown when the config lists
// none.
var defaultKubernetesVersions = []string{"v1.36.2", "v1.35.2"}

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
	for minor, rel := range cfg.Containerd {
		if !reK8sMinor.MatchString(minor) {
			return nil, fmt.Errorf("containerd: key %q must be a Kubernetes minor like v1.35", minor)
		}
		if !reSemver.MatchString(rel.Containerd) || !reSemver.MatchString(rel.Runc) {
			return nil, fmt.Errorf("containerd.%s: containerd and runc must be versions like 2.1.6 and 1.3.4", minor)
		}
		if rel.FIPSSHA256 != "" && !reSHA256.MatchString(rel.FIPSSHA256) {
			return nil, fmt.Errorf("containerd.%s.fipsSha256 must be a lowercase hex sha256", minor)
		}
		for osID, b := range rel.Bundles {
			if b.URL == "" && !reDistro.MatchString(b.Distro) {
				return nil, fmt.Errorf("containerd.%s.bundles.%s: distro must look like rocky-9.8, or set url", minor, osID)
			}
			if u, err := url.Parse(b.URL); b.URL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
				return nil, fmt.Errorf("containerd.%s.bundles.%s.url must be an http(s) URL", minor, osID)
			}
			if !reSHA256.MatchString(b.SHA256) {
				return nil, fmt.Errorf("containerd.%s.bundles.%s.sha256 must be a lowercase hex sha256", minor, osID)
			}
		}
	}
	if a := cfg.Airgap; a.Enabled {
		if a.ArtifactsDir == "" || a.TokenSecret == "" {
			return nil, fmt.Errorf("airgap.artifactsDir and airgap.tokenSecret must be set when airgap.enabled")
		}
		if u, err := url.Parse(a.ServiceURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("airgap.serviceURL must be the http(s) URL build pods reach this service at")
		}
		if _, err := resource.ParseQuantity(a.BuildVolumeSizeLimit); err != nil {
			return nil, fmt.Errorf("airgap.buildVolumeSizeLimit: %w", err)
		}
		if a.MaxUploadBytes < 1 {
			return nil, fmt.Errorf("airgap.maxUploadBytes must be positive")
		}
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
	if c.Containerd == nil {
		c.Containerd = map[string]ContainerdRelease{}
	}
	for minor, rel := range defaultContainerdReleases {
		if _, ok := c.Containerd[minor]; !ok {
			c.Containerd[minor] = rel
		}
	}
	if c.Airgap.BuildVolumeSizeLimit == "" {
		c.Airgap.BuildVolumeSizeLimit = "40Gi"
	}
	if c.Airgap.MaxUploadBytes == 0 {
		c.Airgap.MaxUploadBytes = 20 << 30
	}
	c.Airgap.ServiceURL = strings.TrimSuffix(c.Airgap.ServiceURL, "/")
	d := &c.Defaults
	if d.ImageNameTemplate == "" {
		d.ImageNameTemplate = defaultImageNameTemplate
	}
	if len(c.Catalog.KubernetesVersions) == 0 {
		c.Catalog.KubernetesVersions = slices.Clone(defaultKubernetesVersions)
	}
	// The preselected version is the first one in the dropdown.
	if d.KubernetesVersion == "" {
		d.KubernetesVersion = c.Catalog.KubernetesVersions[0]
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
