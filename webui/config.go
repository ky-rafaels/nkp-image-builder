package main

import (
	"encoding/json"
	"fmt"
	"os"

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
	d := &c.Defaults
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
