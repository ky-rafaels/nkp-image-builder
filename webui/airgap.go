package main

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Air-gapped builds install only from the artifact store (artifacts.go): the
// build pod downloads the OS package bundles, NKP's containerd bundle and
// image bundle for the build into an emptyDir, and the build VM gets
// everything from there over SSH (images/capi/ansible/roles/airgap). The VM
// boots from an image already in Prism Central, so the build needs nothing
// but Prism Central. Harvesting builds are online builds that also save the
// packages they install as an OS package bundle in the store, for air-gapped
// builds of the same OS, base image and options.

const (
	airgapMountPath    = "/airgap"
	airgapArtifactsDir = airgapMountPath + "/artifacts"
	airgapHarvestDir   = airgapMountPath + "/harvest"
	airgapVolume       = "airgap"
	airgapFetchName    = "fetch-artifacts"
	airgapTokenKey     = "token"
)

// airgapOS is how an OS ID's artifacts are named: OS package bundles
// <k8s>_<bundleOS>_<version>..., containerd bundles
// containerd-<version>-d2iq.1-<distro>.... Versions are prefixes ("9." for
// any EL9 minor, which the base image decides).
type airgapOS struct {
	bundleOS      string
	versionPrefix string
	distros       []string
}

var reAirgapOS = regexp.MustCompile(`^(ubuntu)-(\d{2})(\d{2})$|^(rockylinux|rhel)-(\d+)$`)

func airgapOSFor(osID string) (airgapOS, bool) {
	m := reAirgapOS.FindStringSubmatch(osID)
	switch {
	case m == nil:
		return airgapOS{}, false
	case m[1] == "ubuntu":
		v := m[2] + "." + m[3]
		return airgapOS{"ubuntu", v, []string{"ubuntu-" + v}}, true
	case m[4] == "rockylinux":
		// NKP has no Rocky FIPS containerd bundles; any EL9 bundle runs on Rocky.
		return airgapOS{"rocky", m[5] + ".", []string{"rocky-" + m[5] + ".", "rhel-" + m[5] + "."}}, true
	default:
		return airgapOS{"redhat", m[5] + ".", []string{"rhel-" + m[5] + "."}}, true
	}
}

// validateAirgap checks the air-gapped and harvesting options that don't
// depend on the store's contents (airgapPreflight checks those).
func (r *BuildRequest) validateAirgap(cfg *Config, osOpt OSOption, osOK bool, errs fieldErrors) {
	if !r.Airgap && !r.HarvestPackages {
		if len(r.OSPackageBundles) > 0 {
			errs["osPackageBundles"] = "OS package bundles are only used by air-gapped builds"
		}
		return
	}
	switch {
	case !cfg.Airgap.Enabled:
		errs["airgap"] = "air-gapped builds are not enabled on this service"
		return
	case r.Airgap && r.HarvestPackages:
		errs["airgap"] = "an air-gapped build can't also harvest packages: harvest with an online build"
		return
	}
	if _, ok := airgapOSFor(r.OS); osOK && !ok {
		errs["airgap"] = osOpt.Label + " can't be built air-gapped here; use Ubuntu, RHEL or Rocky Linux"
	}
	if !r.Airgap {
		return
	}
	if r.SourceImage == "" {
		errs["sourceImage"] = "air-gapped builds boot from an image already in Prism Central: upload the base image there, " +
			"the same one the OS package bundle was harvested from"
	}
	if g := r.GPU; g != nil && g.Mode != gpuModeVGPU && packageFamily(r.OS) == "rpm" {
		errs["gpu"] = "air-gapped GPU builds on RHEL/Rocky need a vGPU profile: NVIDIA's data center driver for them comes from a modular repo that can't be bundled"
	}
	for _, b := range r.OSPackageBundles {
		if artifactKind(b) != artifactOSPackages {
			errs["osPackageBundles"] = fmt.Sprintf("%q is not an OS package bundle (<k8s>_<os>_<version>_x86_64….tar.gz)", b)
		}
	}
}

// airgapPreflight picks the store's artifacts an air-gapped build needs,
// which its pod downloads (r.airgapFiles), or says what's missing.
func (s *server) airgapPreflight(r *BuildRequest) fieldErrors {
	if !r.Airgap {
		return nil
	}
	if s.artifacts == nil {
		return fieldErrors{"airgap": "air-gapped builds are not enabled on this service"}
	}
	l, err := s.artifacts.list()
	if err != nil {
		return fieldErrors{"airgap": "reading the artifact store failed: " + err.Error()}
	}
	byName := map[string]Artifact{}
	for _, a := range l.Artifacts {
		byName[a.Name] = a
	}
	osn, _ := airgapOSFor(r.OS)
	k8s := strings.TrimPrefix(r.KubernetesVersion, "v")
	isFIPS := func(name string) bool { return strings.Contains(name, "_fips") || strings.Contains(name, "-fips") }
	errs := fieldErrors{}
	var files []Artifact

	// OS package bundles: those named, else every one for this Kubernetes
	// version and OS major; Ansible picks the one for the base image's
	// version and the build's options.
	if len(r.OSPackageBundles) > 0 {
		for _, n := range r.OSPackageBundles {
			a, ok := byName[n]
			if !ok {
				errs["osPackageBundles"] = fmt.Sprintf("%s is not in the artifact store", n)
				continue
			}
			files = append(files, a)
		}
	} else {
		prefix := k8s + "_" + osn.bundleOS + "_" + osn.versionPrefix
		var found []Artifact
		for _, a := range l.Artifacts {
			if a.Kind == artifactOSPackages && strings.HasPrefix(a.Name, prefix) && isFIPS(a.Name) == r.FIPS {
				found = append(found, a)
			}
		}
		if len(found) == 0 {
			errs["osPackageBundles"] = fmt.Sprintf("the artifact store has no%s OS package bundle for Kubernetes %s on %s (%s…): "+
				"run a harvesting build of the same OS, base image and options first, or upload one",
				map[bool]string{true: " FIPS"}[r.FIPS], k8s, osLabelFor(s.cfg, r.OS), prefix)
		}
		files = append(files, found...)
	}

	// NKP's containerd bundle for the build's containerd version and OS.
	cv := r.containerdVersion(s.cfg)
	var ctr []Artifact
	for _, a := range l.Artifacts {
		if a.Kind != artifactContainerd || isFIPS(a.Name) != r.FIPS {
			continue
		}
		for _, d := range osn.distros {
			p := "containerd-"
			if cv != "" {
				p += cv + "-d2iq.1-"
			}
			if strings.HasPrefix(a.Name, p) && strings.Contains(a.Name, "-d2iq.1-"+d) {
				ctr = append(ctr, a)
				break
			}
		}
	}
	if len(ctr) == 0 {
		errs["airgap"] = fmt.Sprintf("the artifact store has no%s containerd %s bundle for %s: upload containerd-%s-d2iq.1-%s…x86_64%s.tar.gz "+
			"from the image-artifacts directory of the NKP release for Kubernetes %s",
			map[bool]string{true: " FIPS"}[r.FIPS], cv, osLabelFor(s.cfg, r.OS), cv, osn.distros[0],
			map[bool]string{true: "_fips"}[r.FIPS], k8s)
	}
	files = append(files, ctr...)

	// NKP's image bundle for the Kubernetes version.
	var imgs []Artifact
	for _, a := range l.Artifacts {
		if a.Kind == artifactImages && strings.HasPrefix(a.Name, "images/kubernetes-images-"+k8s+"-") && isFIPS(a.Name) == r.FIPS {
			imgs = append(imgs, a)
		}
	}
	if len(imgs) == 0 {
		msg := fmt.Sprintf("the artifact store has no image bundle for Kubernetes %s: upload images/kubernetes-images-%s-d2iq.1%s.tar "+
			"from the NKP release's image-artifacts directory", k8s, k8s, map[bool]string{true: "-fips"}[r.FIPS])
		if prev := errs["airgap"]; prev != "" {
			msg = prev + "; " + msg
		}
		errs["airgap"] = msg
	}
	files = append(files, imgs...)

	// The vGPU guest driver, by its configured URL's file name.
	if g := r.GPU; g != nil && g.Mode == gpuModeVGPU {
		if d, ok := s.cfg.GPU.NVIDIA.vgpuDriverFor(r.OS, r.STIG); ok {
			n := path.Base(d.URL)
			if a, ok := byName[n]; ok {
				files = append(files, a)
			} else {
				errs["gpu"] = fmt.Sprintf("air-gapped vGPU builds take the vGPU guest driver from the artifact store: upload %s", n)
			}
		}
	}

	if len(errs) > 0 {
		return errs
	}
	r.airgapFiles = files
	return nil
}

func osLabelFor(cfg *Config, id string) string {
	if o, ok := cfg.osByID(id); ok {
		return o.Label
	}
	return id
}

// containerdVersion is the containerd the build installs, or "" when the
// Packer default applies.
func (r *BuildRequest) containerdVersion(cfg *Config) string {
	vars := r.packerVars(cfg)
	if r.FIPS {
		if v := vars["fips_containerd_version"]; v != "" {
			return v
		}
		return cfg.FIPS.ContainerdVersion
	}
	return vars["containerd_version"]
}

// airgapPackerVars point the build at the downloaded artifacts, or at where
// a harvesting build leaves its bundle and uploads it.
func (r *BuildRequest) airgapPackerVars(cfg *Config, vars map[string]string) {
	switch {
	case r.Airgap:
		vars["airgap_artifacts_dir"] = airgapArtifactsDir
		if len(r.OSPackageBundles) > 0 {
			vars["airgap_os_package_bundles"] = strings.Join(r.OSPackageBundles, " ")
		}
	case r.HarvestPackages:
		vars["airgap_harvest_dir"] = airgapHarvestDir
		vars["airgap_harvest_upload_url"] = cfg.Airgap.ServiceURL + "/api/artifacts"
	}
}

// fetchArtifactsScript downloads $IB_ARTIFACTS ("<sha256> <name>" lines) from
// the artifact store and checks them.
const fetchArtifactsScript = `set -euo pipefail
umask 077
printf 'Authorization: Bearer %s\n' "$IB_ARTIFACTS_TOKEN" > /tmp/artifacts-auth
umask 022
mkdir -p "$IB_ARTIFACTS_DIR/images"
while read -r sum name; do
  [ -n "$name" ] || continue
  echo "[webui] downloading $name from the artifact store"
  curl --fail --silent --show-error --retry 5 --retry-all-errors --header @/tmp/artifacts-auth \
    --output "$IB_ARTIFACTS_DIR/$name" "$IB_ARTIFACTS_URL/$name"
  if [ "$sum" != "-" ]; then
    echo "$sum  $IB_ARTIFACTS_DIR/$name" | sha256sum --check --quiet -
  fi
done <<< "$IB_ARTIFACTS"
rm -f /tmp/artifacts-auth
echo "[webui] artifacts ready:"
ls -lR "$IB_ARTIFACTS_DIR"`

func artifactsTokenEnv(cfg *Config) corev1.EnvVar {
	return corev1.EnvVar{Name: "IB_ARTIFACTS_TOKEN", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: cfg.Airgap.TokenSecret},
			Key:                  airgapTokenKey,
		},
	}}
}

// airgapPod adds the artifacts volume, the init container that downloads an
// air-gapped build's artifacts and, for harvesting, the token the build
// uploads its bundle with.
func airgapPod(cfg *Config, req *BuildRequest, spec *corev1.PodSpec) {
	if !req.Airgap && !req.HarvestPackages {
		return
	}
	limit := resource.MustParse(cfg.Airgap.BuildVolumeSizeLimit)
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name:         airgapVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit}},
	})
	main := &spec.Containers[0]
	main.VolumeMounts = append(main.VolumeMounts, corev1.VolumeMount{Name: airgapVolume, MountPath: airgapMountPath})
	if req.HarvestPackages {
		main.Env = append(main.Env, artifactsTokenEnv(cfg))
		return
	}
	var lines []string
	for _, a := range req.airgapFiles {
		sum := a.SHA256
		if sum == "" {
			sum = "-"
		}
		lines = append(lines, sum+" "+a.Name)
	}
	spec.InitContainers = append(spec.InitContainers, corev1.Container{
		Name:            airgapFetchName,
		Image:           cfg.Builder.Image,
		ImagePullPolicy: cfg.Builder.ImagePullPolicy,
		Command:         []string{"/bin/bash", "-c", fetchArtifactsScript},
		Env: []corev1.EnvVar{
			{Name: "IB_ARTIFACTS", Value: strings.Join(lines, "\n")},
			{Name: "IB_ARTIFACTS_DIR", Value: airgapArtifactsDir},
			{Name: "IB_ARTIFACTS_URL", Value: cfg.Airgap.ServiceURL + "/api/artifacts"},
			artifactsTokenEnv(cfg),
		},
		SecurityContext: cfg.Builder.SecurityContext,
		VolumeMounts:    []corev1.VolumeMount{{Name: airgapVolume, MountPath: airgapMountPath}},
	})
}
