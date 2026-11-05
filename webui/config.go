package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

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
	ImageNamePrefix   string `json:"imageNamePrefix"`
	// ImageNameTemplate builds the default image name. Placeholders:
	// {prefix} ImageNamePrefix, {os} OSOption.ImageNameOS (e.g. ubuntu-24.04),
	// {version} Kubernetes version without the "v" (e.g. 1.36.1),
	// {timestamp} UTC build time as YYYYMMDDhhmmss,
	// {fips} "-fips" for FIPS builds, "" otherwise.
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
}

func (c *Config) osByID(id string) (OSOption, bool) {
	for _, o := range c.Catalog.OperatingSystems {
		if o.ID == id {
			return o, true
		}
	}
	return OSOption{}, false
}
