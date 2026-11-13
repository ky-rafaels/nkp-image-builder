package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func gpuTestConfig() *Config {
	cfg := testConfig()
	cfg.GPU = GPUConfig{Enabled: true, Discovery: true}
	for i := range cfg.Catalog.OperatingSystems {
		cfg.Catalog.OperatingSystems[i].GPUCapable = true
	}
	cfg.Catalog.OperatingSystems = append(cfg.Catalog.OperatingSystems, OSOption{ID: "flatcar", Label: "Flatcar", RequiresImageURL: true})
	return cfg
}

func TestGPUNormalizeAndPackerVars(t *testing.T) {
	cfg := gpuTestConfig()
	open := false
	cfg.GPU.NVIDIA = NVIDIAConfig{DriverBranch: "570", OpenKernelModules: &open, RPMRepo: "https://mirror/cuda/rhel9/x86_64",
		VGPUDriver: VGPUDrivers{
			Run: VGPUDriver{URL: "https://mirror/NVIDIA-Linux-x86_64-580-grid.run", SHA256: "run123"},
			RPM: VGPUDriver{URL: "https://mirror/nvidia-linux-grid-580.rpm", SHA256: "abc123"},
		}}

	r := &BuildRequest{OS: "ubuntu-2204", KubernetesVersion: "v1.36.1", GPU: &GPURequest{Name: "  NVIDIA L40S "}}
	if err := r.normalize(cfg); err != nil {
		t.Fatal(err)
	}
	if r.GPU.Name != "NVIDIA L40S" || r.GPU.Mode != gpuModePassthrough {
		t.Errorf("gpu = %+v", r.GPU)
	}
	if r.ImageName != "nkp-ubuntu-22.04-gpu-release-1.36.1-20261105193022.qcow2" {
		t.Errorf("image name = %q", r.ImageName)
	}
	v := r.packerVars(cfg)
	for k, want := range map[string]string{
		"nutanix_gpu_name":               "NVIDIA L40S",
		"gpu_nvidia_driver_branch":       "570",
		"gpu_nvidia_open_kernel_modules": "false",
		"gpu_nvidia_rpm_repo":            "https://mirror/cuda/rhel9/x86_64",
	} {
		if v[k] != want {
			t.Errorf("%s = %q, want %q", k, v[k], want)
		}
	}
	if _, ok := v["gpu_nvidia_vgpu_driver_url"]; ok {
		t.Error("passthrough builds must not get the vGPU driver")
	}

	r = &BuildRequest{OS: "rockylinux-9", KubernetesVersion: "v1.36.1", STIG: true, GPU: &GPURequest{Name: "NVIDIA A40-8Q", Mode: "vGPU"}}
	if err := r.normalize(cfg); err != nil {
		t.Fatal(err)
	}
	if r.ImageName != "nkp-rockylinux-9-fips-stig-vgpu-release-1.36.1-20261105193022.qcow2" {
		t.Errorf("image name = %q", r.ImageName)
	}
	v = r.packerVars(cfg)
	if v["gpu_nvidia_vgpu_driver_url"] != "https://mirror/nvidia-linux-grid-580.rpm" || v["gpu_nvidia_vgpu_driver_sha256"] != "abc123" {
		t.Errorf("vgpu vars: %v", v)
	}
	// Ubuntu has no .deb configured, so it gets the .run.
	r = &BuildRequest{OS: "ubuntu-2204", GPU: &GPURequest{Name: "NVIDIA A40-8Q", Mode: "vgpu"}}
	if err := r.normalize(cfg); err != nil {
		t.Fatal(err)
	}
	if v := r.packerVars(cfg); v["gpu_nvidia_vgpu_driver_url"] != "https://mirror/NVIDIA-Linux-x86_64-580-grid.run" || v["gpu_nvidia_vgpu_driver_sha256"] != "run123" {
		t.Errorf("ubuntu vgpu vars: %v", v)
	}

	// A blank name means no GPU.
	r = &BuildRequest{OS: "ubuntu-2204", GPU: &GPURequest{Name: " ", Mode: "vgpu"}}
	if err := r.normalize(cfg); err != nil || r.GPU != nil {
		t.Errorf("blank gpu: err=%v gpu=%+v", err, r.GPU)
	}
	if _, ok := r.packerVars(cfg)["nutanix_gpu_name"]; ok {
		t.Error("no GPU must leave nutanix_gpu_name unset")
	}
}

func TestGPURejects(t *testing.T) {
	runURL := func(c *Config) { c.GPU.NVIDIA.VGPUDriver.Run.URL = "https://mirror/NVIDIA-Linux-x86_64-580-grid.run" }
	for name, c := range map[string]struct {
		req   BuildRequest
		cfg   func(*Config)
		field string
	}{
		"disabled":            {BuildRequest{OS: "ubuntu-2204", GPU: &GPURequest{Name: "NVIDIA L4"}}, func(c *Config) { c.GPU.Enabled = false }, "gpu"},
		"os not capable":      {BuildRequest{OS: "flatcar", SourceImage: "flatcar", GPU: &GPURequest{Name: "NVIDIA L4"}}, nil, "gpu"},
		"quote in name":       {BuildRequest{OS: "ubuntu-2204", GPU: &GPURequest{Name: `L4" bogus`}}, nil, "gpu"},
		"bad mode":            {BuildRequest{OS: "ubuntu-2204", GPU: &GPURequest{Name: "NVIDIA L4", Mode: "sriov"}}, nil, "gpu"},
		"vgpu without driver": {BuildRequest{OS: "ubuntu-2204", GPU: &GPURequest{Name: "NVIDIA A40-8Q", Mode: "vgpu"}}, nil, "gpu"},
		"stig vgpu .run":      {BuildRequest{OS: "rockylinux-9", STIG: true, GPU: &GPURequest{Name: "NVIDIA A40-8Q", Mode: "vgpu"}}, runURL, "gpu"},
		"gpu extra var":       {BuildRequest{OS: "ubuntu-2204", ExtraVars: map[string]string{"gpu_nvidia_vgpu_driver_url": "https://evil/x.run"}}, nil, "extraVars"},
		"gpu name extra var":  {BuildRequest{OS: "ubuntu-2204", ExtraVars: map[string]string{"nutanix_gpu_name": "NVIDIA L4"}}, nil, "extraVars"},
	} {
		cfg := gpuTestConfig()
		if c.cfg != nil {
			c.cfg(cfg)
		}
		err := c.req.normalize(cfg)
		if fe, ok := err.(fieldErrors); !ok || fe[c.field] == "" {
			t.Errorf("%s: want %s error, got %v", name, c.field, err)
		}
	}
	// The same vGPU with a .run driver is fine without STIG.
	cfg := gpuTestConfig()
	runURL(cfg)
	r := &BuildRequest{OS: "rockylinux-9", GPU: &GPURequest{Name: "NVIDIA A40-8Q", Mode: "vgpu"}}
	if err := r.normalize(cfg); err != nil {
		t.Errorf("non-STIG vGPU with .run: %v", err)
	}
}

func TestLoadConfigValidatesGPU(t *testing.T) {
	for name, body := range map[string]string{
		"branch": `{"gpu":{"nvidia":{"driverBranch":"latest"}}}`,
		"url":    `{"gpu":{"nvidia":{"vgpuDriver":{"run":{"url":"file:///tmp/x.run"}}}}}`,
		"ext":    `{"gpu":{"nvidia":{"vgpuDriver":{"rpm":{"url":"https://mirror/x.run"}}}}}`,
	} {
		path := t.TempDir() + "/config.json"
		cfg := `{"builder":{"image":"x"},"nutanix":{"credentialsSecret":"s"},"catalog":{"operatingSystems":[{"id":"ubuntu-2204"}]},` + body[1:]
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "gpu.nvidia") {
			t.Errorf("%s: want a gpu.nvidia error, got %v", name, err)
		}
	}
}

// fakePrism serves the v3 hosts list two hosts per page, like a paging
// Prism Central.
func fakePrism(t *testing.T, hosts []map[string]any, calls *atomic.Int32, status int) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/nutanix/v3/hosts/list" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("svc:pw")) {
			t.Errorf("auth header = %q", got)
		}
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		var body struct {
			Kind   string `json:"kind"`
			Offset int    `json:"offset"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Kind != "host" {
			t.Errorf("kind = %q", body.Kind)
		}
		end := min(body.Offset+2, len(hosts))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entities": hosts[body.Offset:end],
			"metadata": map[string]any{"total_matches": len(hosts)},
		})
	}))
}

func host(cluster string, gpus ...map[string]any) map[string]any {
	return map[string]any{"status": map[string]any{
		"name":              "host",
		"cluster_reference": map[string]any{"kind": "cluster", "name": cluster, "uuid": "uuid-" + cluster},
		"resources":         map[string]any{"gpu_list": gpus},
	}}
}

func gpu(name, vendor, mode string, assignable bool) map[string]any {
	return map[string]any{"name": name, "vendor": vendor, "mode": mode, "assignable": assignable, "status": "UNUSED"}
}

func TestPrismClientClusterGPUs(t *testing.T) {
	hosts := []map[string]any{
		host("SA-GPU", gpu("NVIDIA L40S", "kNvidia", "PASSTHROUGH_COMPUTE", true), gpu("NVIDIA L40S", "kNvidia", "PASSTHROUGH_COMPUTE", false)),
		host("SA-GPU", gpu("NVIDIA A40-8Q", "NVIDIA", "VIRTUAL", true), gpu("NVIDIA A40-8Q", "NVIDIA", "VIRTUAL", true)),
		host("SA-GPU"), // no GPUs
		host("other", gpu("Tesla T4", "kNvidia", "PASSTHROUGH_COMPUTE", true)),
		map[string]any{"status": map[string]any{"name": "orphan"}}, // no cluster reference
	}
	var calls atomic.Int32
	srv := fakePrism(t, hosts, &calls, http.StatusOK)
	defer srv.Close()
	pc := &prismClient{baseURL: srv.URL, username: "svc", password: "pw", http: srv.Client(), ttl: time.Minute}

	got, err := pc.ClusterGPUs(context.Background(), "sa-gpu")
	if err != nil {
		t.Fatal(err)
	}
	want := []ClusterGPU{
		{Name: "NVIDIA L40S", Vendor: "NVIDIA", Mode: gpuModePassthrough, PrismMode: "PASSTHROUGH_COMPUTE", Total: 2, Available: 1},
		{Name: "NVIDIA A40-8Q", Vendor: "NVIDIA", Mode: gpuModeVGPU, PrismMode: "VIRTUAL", Total: 2, Available: 2},
	}
	if !got.ClusterFound || len(got.GPUs) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got.GPUs[i] != want[i] {
			t.Errorf("gpu %d = %+v, want %+v", i, got.GPUs[i], want[i])
		}
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("expected 3 pages, got %d requests", n)
	}
	// Cached: other clusters come from the same host list.
	other, err := pc.ClusterGPUs(context.Background(), "other")
	if err != nil || len(other.GPUs) != 1 || other.GPUs[0].Name != "Tesla T4" || calls.Load() != 3 {
		t.Errorf("other = %+v, err %v, calls %d", other, err, calls.Load())
	}
	missing, _ := pc.ClusterGPUs(context.Background(), "nope")
	if missing.ClusterFound || missing.GPUs == nil {
		t.Errorf("missing cluster = %+v (GPUs must encode as [], not null)", missing)
	}

	var bad atomic.Int32
	denied := fakePrism(t, nil, &bad, http.StatusUnauthorized)
	defer denied.Close()
	pc = &prismClient{baseURL: denied.URL, username: "svc", password: "pw", http: denied.Client(), ttl: time.Minute}
	if _, err := pc.ClusterGPUs(context.Background(), "SA-GPU"); err == nil || !strings.Contains(err.Error(), "rejected the credentials") {
		t.Errorf("401: %v", err)
	}
}

func TestNewPrismClientFromEnv(t *testing.T) {
	t.Setenv("NUTANIX_ENDPOINT", "pc.example.com")
	t.Setenv("NUTANIX_USERNAME", "svc")
	t.Setenv("NUTANIX_PASSWORD", "pw")
	t.Setenv("NUTANIX_PORT", "")
	t.Setenv("NUTANIX_INSECURE", "true")
	pc, err := newPrismClientFromEnv()
	if err != nil || pc.baseURL != "https://pc.example.com:9440" {
		t.Fatalf("pc = %+v, err %v", pc, err)
	}
	t.Setenv("NUTANIX_ENDPOINT", "fd00::10")
	if pc, _ := newPrismClientFromEnv(); pc.baseURL != "https://[fd00::10]:9440" {
		t.Errorf("ipv6 base URL = %q", pc.baseURL)
	}
	t.Setenv("NUTANIX_ENDPOINT", "https://pc.example.com")
	if _, err := newPrismClientFromEnv(); err == nil {
		t.Error("an endpoint with a scheme must be rejected")
	}
	t.Setenv("NUTANIX_ENDPOINT", "")
	if _, err := newPrismClientFromEnv(); err == nil {
		t.Error("missing endpoint must be an error")
	}
}

type fakeGPUs struct {
	list *ClusterGPUs
	err  error
}

func (f fakeGPUs) ClusterGPUs(_ context.Context, cluster string) (*ClusterGPUs, error) {
	if f.err != nil {
		return nil, f.err
	}
	l := *f.list
	l.Cluster = cluster
	l.ClusterFound = l.ClusterFound && strings.EqualFold(cluster, "SA-GPU")
	return &l, nil
}

func newGPUTestServer(t *testing.T, lister gpuLister) *server {
	s, _ := newTestServer(t)
	s.cfg = gpuTestConfig()
	s.builds.cfg = s.cfg
	s.cfg.GPU.NVIDIA.VGPUDriver.DEB.URL = "https://mirror/nvidia-linux-grid-580.deb"
	s.gpus = lister
	return s
}

var testClusterGPUs = &ClusterGPUs{ClusterFound: true, GPUs: []ClusterGPU{
	{Name: "NVIDIA L40S", Vendor: "NVIDIA", Mode: gpuModePassthrough, Total: 2, Available: 1},
	{Name: "NVIDIA H100", Vendor: "NVIDIA", Mode: gpuModePassthrough, Total: 1, Available: 0},
	{Name: "NVIDIA A40-8Q", Vendor: "NVIDIA", Mode: gpuModeVGPU, Total: 4, Available: 4},
	{Name: "Radeon Pro V620", Vendor: "AMD", Mode: gpuModePassthrough, Total: 1, Available: 1},
}}

func TestGPUAPI(t *testing.T) {
	get := func(s *server, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	s := newGPUTestServer(t, fakeGPUs{list: testClusterGPUs})
	rec := get(s, "/api/gpus?cluster=SA-GPU")
	var list ClusterGPUs
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.GPUs) != 4 || list.Cluster != "SA-GPU" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := get(s, "/api/gpus"); rec.Code != http.StatusBadRequest {
		t.Errorf("no cluster: %d", rec.Code)
	}
	var cfg configResponse
	_ = json.Unmarshal(get(s, "/api/config").Body.Bytes(), &cfg)
	if !cfg.GPU.Enabled || !cfg.GPU.Discovery || !cfg.GPU.VGPUDriver["deb"] || cfg.GPU.VGPUDriver["rpm"] || cfg.GPU.DriverBranch != "580" {
		t.Errorf("config gpu = %+v", cfg.GPU)
	}
	if strings.Contains(get(s, "/api/config").Body.String(), "mirror/nvidia") {
		t.Error("/api/config must not expose the vGPU driver URL")
	}

	s = newGPUTestServer(t, fakeGPUs{err: errors.New("Prism Central 500")})
	if rec := get(s, "/api/gpus?cluster=SA-GPU"); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Prism Central 500") {
		t.Errorf("upstream error: %d %s", rec.Code, rec.Body)
	}
	s = newGPUTestServer(t, nil)
	if rec := get(s, "/api/gpus?cluster=SA-GPU"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no credentials: %d", rec.Code)
	}
	_ = json.Unmarshal(get(s, "/api/config").Body.Bytes(), &cfg)
	if cfg.GPU.Discovery {
		t.Error("discovery must be reported off without a Prism client")
	}
	s.cfg.GPU.Discovery = false
	if rec := get(s, "/api/gpus?cluster=SA-GPU"); rec.Code != http.StatusNotFound {
		t.Errorf("discovery disabled: %d", rec.Code)
	}
}

func TestGPUPreflight(t *testing.T) {
	post := func(s *server, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/builds", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		return rec
	}
	ok := fakeGPUs{list: testClusterGPUs}
	for name, c := range map[string]struct {
		lister gpuLister
		body   string
		code   int
		want   string
	}{
		"free passthrough":   {ok, `{"os":"ubuntu-2204","clusterName":"SA-GPU","gpu":{"name":"nvidia l40s","mode":"passthrough"}}`, 201, ""},
		"free vgpu":          {ok, `{"os":"ubuntu-2204","clusterName":"SA-GPU","gpu":{"name":"NVIDIA A40-8Q","mode":"vgpu"}}`, 201, ""},
		"not on cluster":     {ok, `{"os":"ubuntu-2204","clusterName":"SA-GPU","gpu":{"name":"Tesla T4"}}`, 422, "has no GPU named"},
		"all in use":         {ok, `{"os":"ubuntu-2204","clusterName":"SA-GPU","gpu":{"name":"NVIDIA H100"}}`, 422, "in use"},
		"wrong type":         {ok, `{"os":"ubuntu-2204","clusterName":"SA-GPU","gpu":{"name":"NVIDIA A40-8Q","mode":"passthrough"}}`, 422, "is a vGPU profile"},
		"no vgpu driver":     {ok, `{"os":"rockylinux-9","clusterName":"SA-GPU","gpu":{"name":"NVIDIA A40-8Q","mode":"vgpu"}}`, 422, "vgpuDriver"},
		"not nvidia":         {ok, `{"os":"ubuntu-2204","clusterName":"SA-GPU","gpu":{"name":"Radeon Pro V620"}}`, 422, "only NVIDIA"},
		"unknown cluster":    {ok, `{"os":"ubuntu-2204","clusterName":"SA-CPU","gpu":{"name":"NVIDIA L40S"}}`, 422, `"clusterName"`},
		"prism unreachable":  {fakeGPUs{err: errors.New("timeout")}, `{"os":"ubuntu-2204","clusterName":"SA-GPU","gpu":{"name":"Tesla T4"}}`, 201, ""},
		"other prism (skip)": {ok, `{"os":"ubuntu-2204","clusterName":"SA-GPU","gpu":{"name":"Tesla T4"},"credentials":{"endpoint":"pc2.example.com"}}`, 201, ""},
		"no discovery":       {nil, `{"os":"ubuntu-2204","clusterName":"SA-GPU","gpu":{"name":"Tesla T4"}}`, 201, ""},
	} {
		s := newGPUTestServer(t, c.lister)
		rec := post(s, c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}

	// The build's var-file carries the GPU.
	s := newGPUTestServer(t, ok)
	rec := post(s, `{"os":"ubuntu-2204","clusterName":"SA-GPU","gpu":{"name":"NVIDIA A40-8Q","mode":"vgpu"}}`)
	var b Build
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	cm, err := s.builds.client.CoreV1().ConfigMaps("builds").Get(context.Background(), b.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var vars map[string]string
	_ = json.Unmarshal([]byte(cm.Data[varsFileKey]), &vars)
	if vars["nutanix_gpu_name"] != "NVIDIA A40-8Q" || vars["gpu_nvidia_vgpu_driver_url"] != "https://mirror/nvidia-linux-grid-580.deb" {
		t.Errorf("vars = %v", vars)
	}
	if b.Request == nil || b.Request.GPU == nil || b.Request.GPU.Mode != gpuModeVGPU || !strings.Contains(b.ImageName, "-vgpu-") {
		t.Errorf("build = %+v", b)
	}
}
