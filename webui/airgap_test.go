package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestArtifactKind(t *testing.T) {
	for name, want := range map[string]string{
		"containerd-2.3.1-d2iq.1-rocky-9.8-x86_64.tar.gz":     artifactContainerd,
		"containerd-2.1.6-d2iq.1-rhel-9.6-x86_64_fips.tar.gz": artifactContainerd,
		"images/kubernetes-images-1.36.2-d2iq.1.tar":          artifactImages,
		"1.36.2_rocky_9.8_x86_64.tar.gz":                      artifactOSPackages,
		"1.36.2_redhat_9.6_x86_64_fips_stig.tar.gz":           artifactOSPackages,
		"1.36.2_ubuntu_24.04_x86_64_gpu.tar.gz":               artifactOSPackages,
		"NVIDIA-Linux-x86_64-580.65-grid.run":                 artifactOther,
	} {
		if got := artifactKind(name); got != want {
			t.Errorf("artifactKind(%s) = %s, want %s", name, got, want)
		}
	}
}

func TestArtifactStore(t *testing.T) {
	st := &artifactStore{dir: t.TempDir()}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

	a, err := st.put("images/kubernetes-images-1.36.2-d2iq.1.tar", strings.NewReader("images"), sum("images"))
	if err != nil || a.SHA256 != sum("images") || a.Size != 6 || a.Kind != artifactImages {
		t.Fatalf("put = %+v, %v", a, err)
	}
	if _, err := st.put("1.36.2_rocky_9.8_x86_64.tar.gz", strings.NewReader("rpms"), ""); err != nil {
		t.Fatal(err)
	}
	// A failed checksum leaves nothing behind, and keeps an older version.
	if _, err := st.put("1.36.2_rocky_9.8_x86_64.tar.gz", strings.NewReader("corrupt"), sum("other")); !errors.Is(err, errChecksumMismatch) {
		t.Fatalf("mismatch err = %v", err)
	}
	if a, _ := st.stat("1.36.2_rocky_9.8_x86_64.tar.gz"); a.SHA256 != sum("rpms") {
		t.Fatalf("older version replaced: %+v", a)
	}
	for _, bad := range []string{"../etc/passwd", "images/../../x", "a/b", ".hidden", "images/", "x..y/z"} {
		if _, err := st.put(bad, strings.NewReader("x"), ""); !errors.Is(err, errBadArtifactName) {
			t.Errorf("put(%q) err = %v", bad, err)
		}
	}
	l, err := st.list()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range l.Artifacts {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"1.36.2_rocky_9.8_x86_64.tar.gz", "images/kubernetes-images-1.36.2-d2iq.1.tar"}) {
		t.Fatalf("list = %v (temp and checksum files must not show)", names)
	}
	if l.FreeBytes == 0 {
		t.Error("free space not reported")
	}
	if err := st.delete("1.36.2_rocky_9.8_x86_64.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if err := st.delete("1.36.2_rocky_9.8_x86_64.tar.gz"); !errors.Is(err, errNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(st.dir, ".*"))
	if len(left) != 0 {
		t.Fatalf("leftover hidden files: %v", left)
	}
}

func newAirgapTestServer(t *testing.T) (*server, func(names ...string)) {
	t.Helper()
	s, _ := newTestServer(t)
	dir := t.TempDir()
	s.cfg.Airgap = AirgapConfig{Enabled: true, ArtifactsDir: dir, ServiceURL: "http://nkpib.builds.svc:80/", TokenSecret: "nkpib-artifacts"}
	s.cfg.Catalog.OperatingSystems = append(s.cfg.Catalog.OperatingSystems,
		OSOption{ID: "ubuntu-2404", Label: "Ubuntu 24.04", GPUCapable: true},
		OSOption{ID: "flatcar", Label: "Flatcar"})
	s.cfg.applyDefaults()
	s.artifacts = &artifactStore{dir: dir}
	s.artifactsToken = "s3cret"
	add := func(names ...string) {
		for _, n := range names {
			if _, err := s.artifacts.put(n, strings.NewReader(n), ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s, add
}

// The image-artifacts of NKP 2.18.0 and 2.19.1 (containerd bundles for the OSes
// these builds use, and the image bundles).
var nkpArtifacts = []string{
	"containerd-2.1.6-d2iq.1-rocky-9.7-x86_64.tar.gz",
	"containerd-2.1.6-d2iq.1-rhel-9.6-x86_64.tar.gz",
	"containerd-2.1.6-d2iq.1-rhel-9.6-x86_64_fips.tar.gz",
	"containerd-2.1.6-d2iq.1-ubuntu-24.04-x86_64.tar.gz",
	"containerd-2.3.1-d2iq.1-rocky-9.8-x86_64.tar.gz",
	"containerd-2.3.1-d2iq.1-rhel-9.6-x86_64.tar.gz",
	"containerd-2.3.1-d2iq.1-rhel-9.6-x86_64_fips.tar.gz",
	"containerd-2.3.1-d2iq.1-rhel-9.8-x86_64.tar.gz",
	"containerd-2.3.1-d2iq.1-rhel-9.8-x86_64_fips.tar.gz",
	"containerd-2.3.1-d2iq.1-ubuntu-22.04-x86_64.tar.gz",
	"containerd-2.3.1-d2iq.1-ubuntu-24.04-x86_64.tar.gz",
	"images/kubernetes-images-1.35.2-d2iq.1.tar",
	"images/kubernetes-images-1.35.2-d2iq.1-fips.tar",
	"images/kubernetes-images-1.36.2-d2iq.1.tar",
	"images/kubernetes-images-1.36.2-d2iq.1-fips.tar",
}

func createBuild(t *testing.T, s *server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/builds", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func buildJob(t *testing.T, s *server, rec *httptest.ResponseRecorder) (*corev1.PodSpec, map[string]string) {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var b Build
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	client := s.builds.client
	job, err := client.BatchV1().Jobs("builds").Get(context.Background(), b.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm, _ := client.CoreV1().ConfigMaps("builds").Get(context.Background(), b.Name, metav1.GetOptions{})
	vars := map[string]string{}
	_ = json.Unmarshal([]byte(cm.Data[varsFileKey]), &vars)
	return &job.Spec.Template.Spec, vars
}

func envValue(env []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for _, e := range env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func TestAirgapBuild(t *testing.T) {
	cases := map[string]struct {
		body    string
		bundles []string
		want    []string // downloaded artifacts
	}{
		"rocky 1.36": {
			body:    `{"os":"rockylinux-9","kubernetesVersion":"v1.36.2","airgap":true,"sourceImage":"Rocky-9-GenericCloud-9.8"}`,
			bundles: []string{"1.36.2_rocky_9.8_x86_64.tar.gz", "1.36.2_rocky_9.8_x86_64_fips_stig.tar.gz", "1.35.2_rocky_9.8_x86_64.tar.gz"},
			want: []string{"1.36.2_rocky_9.8_x86_64.tar.gz", "containerd-2.3.1-d2iq.1-rocky-9.8-x86_64.tar.gz",
				"containerd-2.3.1-d2iq.1-rhel-9.6-x86_64.tar.gz", "containerd-2.3.1-d2iq.1-rhel-9.8-x86_64.tar.gz",
				"images/kubernetes-images-1.36.2-d2iq.1.tar"},
		},
		"rocky 1.35 (NKP 2.18's rocky-9.7 bundle)": {
			body:    `{"os":"rockylinux-9","kubernetesVersion":"v1.35.2","airgap":true,"sourceImage":"rocky-9"}`,
			bundles: []string{"1.35.2_rocky_9.6_x86_64.tar.gz"},
			want: []string{"1.35.2_rocky_9.6_x86_64.tar.gz", "containerd-2.1.6-d2iq.1-rocky-9.7-x86_64.tar.gz",
				"containerd-2.1.6-d2iq.1-rhel-9.6-x86_64.tar.gz", "images/kubernetes-images-1.35.2-d2iq.1.tar"},
		},
		"rocky FIPS STIG takes the FIPS bundles": {
			body:    `{"os":"rockylinux-9","kubernetesVersion":"v1.36.2","airgap":true,"stig":true,"diskSizeGB":60,"sourceImage":"rocky-9"}`,
			bundles: []string{"1.36.2_rocky_9.8_x86_64.tar.gz", "1.36.2_rocky_9.8_x86_64_fips_stig.tar.gz"},
			want: []string{"1.36.2_rocky_9.8_x86_64_fips_stig.tar.gz", "containerd-2.3.1-d2iq.1-rhel-9.6-x86_64_fips.tar.gz",
				"containerd-2.3.1-d2iq.1-rhel-9.8-x86_64_fips.tar.gz", "images/kubernetes-images-1.36.2-d2iq.1-fips.tar"},
		},
		"ubuntu 24.04 with a named bundle": {
			body:    `{"os":"ubuntu-2404","kubernetesVersion":"v1.36.2","airgap":true,"sourceImage":"noble","osPackageBundles":["1.36.2_ubuntu_24.04_x86_64_custom.tar.gz"]}`,
			bundles: []string{"1.36.2_ubuntu_24.04_x86_64.tar.gz", "1.36.2_ubuntu_24.04_x86_64_custom.tar.gz"},
			want: []string{"1.36.2_ubuntu_24.04_x86_64_custom.tar.gz", "containerd-2.3.1-d2iq.1-ubuntu-24.04-x86_64.tar.gz",
				"images/kubernetes-images-1.36.2-d2iq.1.tar"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, add := newAirgapTestServer(t)
			add(nkpArtifacts...)
			add(c.bundles...)
			spec, vars := buildJob(t, s, createBuild(t, s, c.body))

			if vars["airgap_artifacts_dir"] != "/airgap/artifacts" {
				t.Errorf("airgap_artifacts_dir = %q", vars["airgap_artifacts_dir"])
			}
			if strings.Contains(c.body, "osPackageBundles") != (vars["airgap_os_package_bundles"] != "") {
				t.Errorf("airgap_os_package_bundles = %q", vars["airgap_os_package_bundles"])
			}
			if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != airgapFetchName {
				t.Fatalf("init containers = %+v", spec.InitContainers)
			}
			init := spec.InitContainers[0]
			list, _ := envValue(init.Env, "IB_ARTIFACTS")
			var got []string
			for _, line := range strings.Split(list.Value, "\n") {
				sum, name, _ := strings.Cut(line, " ")
				h := sha256.Sum256([]byte(name))
				if sum != hex.EncodeToString(h[:]) {
					t.Errorf("%s: sha256 %s not passed for checking", name, sum)
				}
				got = append(got, name)
			}
			slices.Sort(got)
			slices.Sort(c.want)
			if !slices.Equal(got, c.want) {
				t.Errorf("downloads\n got %v\nwant %v", got, c.want)
			}
			if u, _ := envValue(init.Env, "IB_ARTIFACTS_URL"); u.Value != "http://nkpib.builds.svc:80/api/artifacts" {
				t.Errorf("IB_ARTIFACTS_URL = %q", u.Value)
			}
			if tok, _ := envValue(init.Env, "IB_ARTIFACTS_TOKEN"); tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef.Name != "nkpib-artifacts" {
				t.Errorf("token env = %+v", tok)
			}
			if _, ok := envValue(spec.Containers[0].Env, "IB_ARTIFACTS_TOKEN"); ok {
				t.Error("the build container doesn't need the token")
			}
			var vol *corev1.Volume
			for i := range spec.Volumes {
				if spec.Volumes[i].Name == airgapVolume {
					vol = &spec.Volumes[i]
				}
			}
			if vol == nil || vol.EmptyDir == nil || vol.EmptyDir.SizeLimit.String() != "40Gi" {
				t.Fatalf("airgap volume = %+v", vol)
			}
		})
	}
}

func TestAirgapBuildRejects(t *testing.T) {
	ok := `"airgap":true,"sourceImage":"rocky-9"`
	for name, c := range map[string]struct {
		body, field, msg string
		noStore          bool
	}{
		"needs a Prism Central image":       {`{"os":"rockylinux-9","airgap":true}`, "sourceImage", "already in Prism Central", false},
		"not with a URL":                    {`{"os":"rockylinux-9","airgap":true,"sourceImageURL":"https://x/y.qcow2"}`, "sourceImage", "already in Prism Central", false},
		"not with harvesting":               {`{"os":"rockylinux-9",` + ok + `,"harvestPackages":true}`, "airgap", "can't also harvest", false},
		"not Flatcar":                       {`{"os":"flatcar",` + ok + `}`, "airgap", "can't be built air-gapped", false},
		"no OS package bundle":              {`{"os":"rockylinux-9","kubernetesVersion":"v1.34.2",` + ok + `}`, "osPackageBundles", "1.34.2_rocky_9.", false},
		"no image bundle for 1.34":          {`{"os":"rockylinux-9","kubernetesVersion":"v1.34.2",` + ok + `}`, "airgap", "kubernetes-images-1.34.2", false},
		"named bundle missing":              {`{"os":"rockylinux-9",` + ok + `,"osPackageBundles":["1.36.2_rocky_9.6_x86_64.tar.gz"]}`, "osPackageBundles", "not in the artifact store", false},
		"named bundle isn't one":            {`{"os":"rockylinux-9",` + ok + `,"osPackageBundles":["containerd-2.3.1-d2iq.1-rocky-9.8-x86_64.tar.gz"]}`, "osPackageBundles", "is not an OS package bundle", false},
		"no bundle for a pinned containerd": {`{"os":"ubuntu-2404",` + ok + `,"extraVars":{"containerd_version":"2.3.2"}}`, "airgap", "containerd 2.3.2", false},
		"RHEL passthrough GPU":              {`{"os":"rockylinux-9",` + ok + `,"gpu":{"name":"NVIDIA L40S"}}`, "gpu", "vGPU profile", false},
		"bundles without air-gapping":       {`{"os":"rockylinux-9","osPackageBundles":["1.36.2_rocky_9.8_x86_64.tar.gz"]}`, "osPackageBundles", "only used by air-gapped", false},
		"off on this service":               {`{"os":"rockylinux-9",` + ok + `}`, "airgap", "not enabled", true},
		"harvest off on this service":       {`{"os":"rockylinux-9","harvestPackages":true}`, "airgap", "not enabled", true},
	} {
		t.Run(name, func(t *testing.T) {
			s, add := newAirgapTestServer(t)
			add(nkpArtifacts...)
			add("1.36.2_rocky_9.8_x86_64.tar.gz", "1.36.2_ubuntu_24.04_x86_64.tar.gz")
			s.cfg.GPU.Enabled = true
			s.cfg.Catalog.OperatingSystems[2].GPUCapable = true
			if c.noStore {
				s.cfg.Airgap.Enabled = false
				s.artifacts = nil
			}
			rec := createBuild(t, s, c.body)
			var resp struct {
				Fields map[string]string `json:"fields"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(resp.Fields[c.field], c.msg) {
				t.Fatalf("got %d %s, want %s containing %q", rec.Code, rec.Body, c.field, c.msg)
			}
		})
	}
}

func TestHarvestBuild(t *testing.T) {
	s, _ := newAirgapTestServer(t)
	spec, vars := buildJob(t, s, createBuild(t, s, `{"os":"ubuntu-2404","kubernetesVersion":"v1.36.2","harvestPackages":true}`))
	if vars["airgap_harvest_dir"] != "/airgap/harvest" || vars["airgap_harvest_upload_url"] != "http://nkpib.builds.svc:80/api/artifacts" {
		t.Fatalf("vars = %v", vars)
	}
	if _, ok := vars["airgap_artifacts_dir"]; ok {
		t.Fatal("a harvesting build is online")
	}
	if len(spec.InitContainers) != 0 {
		t.Fatalf("init containers = %+v", spec.InitContainers)
	}
	if tok, ok := envValue(spec.Containers[0].Env, "IB_ARTIFACTS_TOKEN"); !ok || tok.ValueFrom.SecretKeyRef.Key != "token" {
		t.Fatalf("token env = %+v", tok)
	}
	mounted := slices.ContainsFunc(spec.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == airgapMountPath })
	if !mounted {
		t.Fatal("harvest volume not mounted")
	}
}

// Air-gapped builds skip the FIPS repo check: the FIPS packages come from the
// store.
func TestAirgapSkipsFIPSPreflight(t *testing.T) {
	s, add := newAirgapTestServer(t)
	add(nkpArtifacts...)
	add("1.36.2_rocky_9.8_x86_64_fips.tar.gz")
	s.cfg.FIPS.Preflight = true
	s.fipsCheck = func(context.Context, string) (bool, error) { return false, errors.New("no route to host") }
	rec := createBuild(t, s, `{"os":"rockylinux-9","kubernetesVersion":"v1.36.2","fips":true,"airgap":true,"sourceImage":"rocky-9"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
}

func TestArtifactAPI(t *testing.T) {
	t.Setenv("BASIC_AUTH_USERNAME", "admin")
	t.Setenv("BASIC_AUTH_PASSWORD", "pw")
	s, _ := newAirgapTestServer(t)
	h := s.routes()
	do := func(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	basic := []string{"Authorization", "Basic YWRtaW46cHc="} // admin:pw
	name := "/api/artifacts/images/kubernetes-images-1.36.2-d2iq.1.tar"
	h256 := sha256.Sum256([]byte("layers"))
	sum := hex.EncodeToString(h256[:])

	if rec := do("PUT", name, "layers"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated put: %d", rec.Code)
	}
	if rec := do("PUT", name, "layers", append(basic, "X-Checksum-Sha256", strings.Repeat("0", 64))...); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad checksum put: %d %s", rec.Code, rec.Body)
	}
	if rec := do("PUT", name, "layers", append(basic, "X-Checksum-Sha256", sum)...); rec.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	// Build pods: the token works for the files, not for anything else.
	bearer := []string{"Authorization", "Bearer s3cret"}
	rec := do("GET", name, "", bearer...)
	if rec.Code != 200 || rec.Body.String() != "layers" || rec.Header().Get("X-Checksum-Sha256") != sum {
		t.Fatalf("token get: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	if rec := do("PUT", "/api/artifacts/1.36.2_ubuntu_24.04_x86_64.tar.gz", "debs", bearer...); rec.Code != http.StatusCreated {
		t.Fatalf("token put (harvest): %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/api/artifacts", "/api/builds", "/api/config"} {
		if rec := do("GET", path, "", bearer...); rec.Code != http.StatusUnauthorized {
			t.Errorf("token on %s: %d", path, rec.Code)
		}
	}
	if rec := do("GET", name, "", "Authorization", "Bearer wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	if rec := do("PUT", "/api/artifacts/x.tar", "x", append(basic, "Origin", "https://evil.example")...); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin put: %d", rec.Code)
	}

	rec = do("GET", "/api/artifacts", "", basic...)
	var l artifactList
	_ = json.Unmarshal(rec.Body.Bytes(), &l)
	if rec.Code != 200 || len(l.Artifacts) != 2 || l.Artifacts[1].Kind != artifactImages || l.Artifacts[0].Kind != artifactOSPackages {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := do("DELETE", name, "", basic...); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := do("GET", name, "", basic...); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", rec.Code)
	}
	if rec := do("PUT", "/api/artifacts/..%2Fescape", "x", basic...); rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.cfg.Airgap.ArtifactsDir), "escape")); err == nil {
		t.Fatal("wrote outside the store")
	}

	// Off: the API says so.
	s.artifacts = nil
	if rec := do("GET", "/api/artifacts", "", basic...); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled list: %d", rec.Code)
	}
}

func TestLoadConfigValidatesAirgap(t *testing.T) {
	base := map[string]any{
		"builder": map[string]any{"image": "x"}, "nutanix": map[string]any{"credentialsSecret": "c"},
		"catalog": map[string]any{"operatingSystems": []any{map[string]any{"id": "rockylinux-9"}}},
	}
	for name, c := range map[string]struct {
		airgap map[string]any
		err    string
	}{
		"ok":           {map[string]any{"enabled": true, "artifactsDir": "/data", "serviceURL": "http://svc:80", "tokenSecret": "t"}, ""},
		"off":          {map[string]any{"enabled": false}, ""},
		"no dir":       {map[string]any{"enabled": true, "serviceURL": "http://svc", "tokenSecret": "t"}, "artifactsDir"},
		"bad url":      {map[string]any{"enabled": true, "artifactsDir": "/d", "serviceURL": "svc:80", "tokenSecret": "t"}, "serviceURL"},
		"bad size":     {map[string]any{"enabled": true, "artifactsDir": "/d", "serviceURL": "http://svc", "tokenSecret": "t", "buildVolumeSizeLimit": "lots"}, "buildVolumeSizeLimit"},
		"negative max": {map[string]any{"enabled": true, "artifactsDir": "/d", "serviceURL": "http://svc", "tokenSecret": "t", "maxUploadBytes": -1}, "maxUploadBytes"},
	} {
		t.Run(name, func(t *testing.T) {
			base["airgap"] = c.airgap
			b, _ := json.Marshal(base)
			p := filepath.Join(t.TempDir(), "c.json")
			_ = os.WriteFile(p, b, 0o600)
			cfg, err := loadConfig(p)
			if c.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				if c.airgap["enabled"] == true && (cfg.Airgap.BuildVolumeSizeLimit != "40Gi" || cfg.Airgap.MaxUploadBytes != 20<<30) {
					t.Fatalf("defaults = %+v", cfg.Airgap)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err = %v, want %s", err, c.err)
			}
		})
	}
}

// An air-gapped build's log starts with the artifact downloads.
func TestLogsIncludeInitContainers(t *testing.T) {
	s, client := newTestServer(t)
	ctx := context.Background()
	rec := createBuild(t, s, `{"os":"ubuntu-2204","kubernetesVersion":"v1.36.2"}`)
	var b Build
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: b.Name + "-x", Namespace: "builds", Labels: map[string]string{labelBuildID: b.ID}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: airgapFetchName}},
			Containers:     []corev1.Container{{Name: buildContainer}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{Name: airgapFetchName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: buildContainer,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}},
		},
	}
	if _, err := client.CoreV1().Pods("builds").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Still downloading: the init container's log only.
	var out strings.Builder
	if err := s.builds.Logs(ctx, b.ID, false, &out); err != nil || out.String() != "fake logs" {
		t.Fatalf("downloading: %q, %v", out.String(), err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if _, err := client.CoreV1().Pods("builds").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := s.builds.Logs(ctx, b.ID, true, &out); err != nil || out.String() != "fake logsfake logs" {
		t.Fatalf("building: %q, %v", out.String(), err)
	}
}
