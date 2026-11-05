package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"log/slog"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testConfig() *Config {
	cfg := &Config{
		Namespace: "builds",
		Builder:   BuilderConfig{Image: "registry.example.com/image-builder:test"},
		Nutanix:   NutanixConfig{CredentialsSecret: "nutanix-creds"},
		Defaults:  BuildDefaults{ClusterName: "pe-01", SubnetName: "vlan-100", ImageNamePrefix: "nkp-"},
		Catalog: CatalogConfig{
			OperatingSystems: []OSOption{
				{ID: "ubuntu-2204", Label: "Ubuntu 22.04"},
				{ID: "rhel-9", Label: "RHEL 9", RequiresImageURL: true, RequiresRHSM: true},
			},
		},
		Features: FeatureConfig{AllowCredentialOverride: true, AllowExtraVars: true},
	}
	cfg.applyDefaults()
	now = func() time.Time { return time.Date(2026, 11, 5, 14, 30, 22, 0, time.FixedZone("x", -5*3600)) }
	return cfg
}

func TestNormalizeDefaults(t *testing.T) {
	cfg := testConfig()
	r := &BuildRequest{OS: "ubuntu-2204", KubernetesVersion: " v1.34.2 "}
	if err := r.normalize(cfg); err != nil {
		t.Fatal(err)
	}
	if r.ImageName != "nkp-ubuntu-22.04-release-1.34.2-20261105193022.qcow2" {
		t.Errorf("image name = %q", r.ImageName)
	}
	if r.ClusterName != "pe-01" || r.SubnetName != "vlan-100" || r.CPUs != 2 || r.MemoryMB != 4096 || r.DiskSizeGB != 40 {
		t.Errorf("defaults not applied: %+v", r)
	}
}

func TestDefaultImageName(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	d := BuildDefaults{ImageNamePrefix: "nkp-"}
	cases := map[string]struct {
		d    BuildDefaults
		o    OSOption
		want string
	}{
		"ubuntu":   {d, OSOption{ID: "ubuntu-2404"}, "nkp-ubuntu-24.04-release-1.36.1-20260102030405.qcow2"},
		"rocky":    {d, OSOption{ID: "rockylinux-9"}, "nkp-rockylinux-9-release-1.36.1-20260102030405.qcow2"},
		"override": {d, OSOption{ID: "rhel-9", ImageNameOS: "rhel-9.6"}, "nkp-rhel-9.6-release-1.36.1-20260102030405.qcow2"},
		"template": {BuildDefaults{ImageNameTemplate: "{os}-k8s-{version}"}, OSOption{ID: "flatcar"}, "flatcar-k8s-1.36.1"},
	}
	for name, c := range cases {
		if got := defaultImageName(c.d, c.o, "v1.36.1", ts); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	cfg := testConfig()
	cases := map[string]struct {
		req   BuildRequest
		field string
	}{
		"unknown os":        {BuildRequest{OS: "windows-2022"}, "os"},
		"bad version":       {BuildRequest{OS: "ubuntu-2204", KubernetesVersion: "1.34"}, "kubernetesVersion"},
		"shell in name":     {BuildRequest{OS: "ubuntu-2204", ImageName: "x;rm -rf /"}, "imageName"},
		"rhel needs source": {BuildRequest{OS: "rhel-9"}, "sourceImage"},
		"url and pc image":  {BuildRequest{OS: "ubuntu-2204", SourceImageURL: "https://x/y.img", SourceImage: "base"}, "sourceImage"},
		"no delete source":  {BuildRequest{OS: "ubuntu-2204", ExtraVars: map[string]string{"source_image_delete": "true"}}, "extraVars"},
		"file url":          {BuildRequest{OS: "ubuntu-2204", SourceImageURL: "file:///etc/passwd"}, "sourceImageURL"},
		"denied extra var":  {BuildRequest{OS: "ubuntu-2204", ExtraVars: map[string]string{"custom_post_processor_command": "curl evil"}}, "extraVars"},
		"bad extra var":     {BuildRequest{OS: "ubuntu-2204", ExtraVars: map[string]string{"Foo-Bar": "x"}}, "extraVars"},
		"newline value":     {BuildRequest{OS: "ubuntu-2204", ExtraVars: map[string]string{"containerd_version": "1\n2"}}, "extraVars"},
		"package inject":    {BuildRequest{OS: "ubuntu-2204", ExtraDebs: "vim; curl x|sh"}, "extraDebs"},
		"half creds":        {BuildRequest{OS: "ubuntu-2204", Credentials: &BuildCredentials{Username: "admin"}}, "credentials.password"},
		"endpoint scheme":   {BuildRequest{OS: "ubuntu-2204", Credentials: &BuildCredentials{Endpoint: "https://pc"}}, "credentials.endpoint"},
		"tiny memory":       {BuildRequest{OS: "ubuntu-2204", MemoryMB: 512}, "memoryMB"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := tc.req
			err := r.normalize(cfg)
			fe, ok := err.(fieldErrors)
			if !ok {
				t.Fatalf("expected fieldErrors, got %v", err)
			}
			if _, ok := fe[tc.field]; !ok {
				t.Fatalf("expected error on %q, got %v", tc.field, fe)
			}
		})
	}
}

func TestFeatureFlagsEnforced(t *testing.T) {
	cfg := testConfig()
	cfg.Features = FeatureConfig{}
	r := &BuildRequest{OS: "ubuntu-2204", ExtraVars: map[string]string{"containerd_version": "2.1.4"},
		Credentials: &BuildCredentials{Username: "a", Password: "b"}}
	fe, _ := r.normalize(cfg).(fieldErrors)
	if fe["extraVars"] == "" || fe["credentials"] == "" {
		t.Fatalf("expected both features rejected, got %v", fe)
	}
}

func TestPackerVars(t *testing.T) {
	cfg := testConfig()
	r := &BuildRequest{OS: "ubuntu-2204", KubernetesVersion: "v1.34.2", ExtraDebs: "nfs-common",
		ExtraVars: map[string]string{"containerd_version": "2.1.4"}}
	if err := r.normalize(cfg); err != nil {
		t.Fatal(err)
	}
	v := r.packerVars()
	want := map[string]string{
		"kubernetes_semver":      "v1.34.2",
		"kubernetes_series":      "v1.34",
		"kubernetes_deb_version": "1.34.2-1.1",
		"kubernetes_rpm_version": "1.34.2",
		"crictl_version":         "1.34.0",
		"nutanix_cluster_name":   "pe-01",
		"nutanix_subnet_name":    "vlan-100",
		"image_name":             "nkp-ubuntu-22.04-release-1.34.2-20261105193022.qcow2",
		"cpus":                   "2",
		"memory":                 "4096",
		"disk_size_gb":           "40",
		"extra_debs":             "nfs-common",
		"containerd_version":     "2.1.4",
	}
	for k, w := range want {
		if v[k] != w {
			t.Errorf("%s = %q, want %q", k, v[k], w)
		}
	}
	if _, ok := v["image_url"]; ok {
		t.Error("image_url should be unset so the per-OS default applies")
	}
}

func TestSourceImageFromPrismCentral(t *testing.T) {
	cfg := testConfig()
	byName := &BuildRequest{OS: "rhel-9", SourceImage: "rhel-9.6-kvm-golden"}
	if err := byName.normalize(cfg); err != nil {
		t.Fatalf("existing image should satisfy RHEL's source requirement: %v", err)
	}
	v := byName.packerVars()
	if v["source_image_name"] != "rhel-9.6-kvm-golden" || v["source_image_uuid"] != "" || v["image_url"] != "" {
		t.Errorf("by name: %v", v)
	}

	byUUID := &BuildRequest{OS: "ubuntu-2204", SourceImage: "6F1C2B3A-1234-4ABC-9DEF-0123456789AB"}
	if err := byUUID.normalize(cfg); err != nil {
		t.Fatal(err)
	}
	v = byUUID.packerVars()
	if v["source_image_uuid"] != "6f1c2b3a-1234-4abc-9def-0123456789ab" || v["source_image_name"] != "" {
		t.Errorf("by uuid: %v", v)
	}
}

func TestBuildObjects(t *testing.T) {
	cfg := testConfig()
	insecure := true
	r := &BuildRequest{OS: "rhel-9", SourceImageURL: "https://mirror/rhel.qcow2",
		Credentials: &BuildCredentials{Username: "u", Password: "secret-pw", Insecure: &insecure, RHSMUser: "r", RHSMPass: "rp"}}
	if err := r.normalize(cfg); err != nil {
		t.Fatal(err)
	}
	cm, secret, job, err := buildObjects(cfg, r, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "nkp-ib-deadbeef" || cm.Name != job.Name || secret.Name != job.Name+"-creds" {
		t.Errorf("unexpected names: %s %s %s", job.Name, cm.Name, secret.Name)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if got := c.Args; len(got) != 1 || got[0] != "build-nutanix-rhel-9" {
		t.Errorf("args = %v", got)
	}
	if len(c.EnvFrom) != 2 || c.EnvFrom[0].SecretRef.Name != "nutanix-creds" || c.EnvFrom[1].SecretRef.Name != secret.Name {
		t.Errorf("envFrom order wrong: %+v", c.EnvFrom)
	}
	if secret.StringData["NUTANIX_PASSWORD"] != "secret-pw" || secret.StringData["NUTANIX_INSECURE"] != "true" || secret.StringData["RHSM_PASS"] != "rp" {
		t.Errorf("secret data = %v", secret.StringData)
	}
	if strings.Contains(job.Annotations[annotationReq], "secret-pw") || strings.Contains(job.Annotations[annotationReq], `"rp"`) {
		t.Error("password leaked into job annotation")
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Error("builds must not retry")
	}
	if *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Error("build pod should not mount an API token")
	}
	var vars map[string]string
	if err := json.Unmarshal([]byte(cm.Data[varsFileKey]), &vars); err != nil {
		t.Fatal(err)
	}
	if vars["image_url"] != "https://mirror/rhel.qcow2" {
		t.Errorf("image_url = %q", vars["image_url"])
	}

	// No override -> no per-build secret, single envFrom.
	r2 := &BuildRequest{OS: "ubuntu-2204"}
	_ = r2.normalize(cfg)
	_, s2, j2, _ := buildObjects(cfg, r2, "00000001")
	if s2 != nil || len(j2.Spec.Template.Spec.Containers[0].EnvFrom) != 1 {
		t.Error("unexpected per-build secret")
	}
}

func TestToBuildPhases(t *testing.T) {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelBuildID: "x"}}}
	if p := toBuild(j).Phase; p != "Pending" {
		t.Errorf("phase = %s", p)
	}
	j.Status.Active = 1 // pod exists but e.g. still pulling its image
	if p := toBuild(j).Phase; p != "Pending" {
		t.Errorf("active-but-not-ready phase = %s, want Pending", p)
	}
	ready := int32(1)
	j.Status.Ready = &ready
	if p := toBuild(j).Phase; p != "Running" {
		t.Errorf("phase = %s", p)
	}
	j.Status.Ready = nil
	j.Status.Active = 0
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	b := toBuild(j)
	if b.Phase != "Failed" || b.Message != "BackoffLimitExceeded" {
		t.Errorf("got %s %q", b.Phase, b.Message)
	}
}

func newTestServer(t *testing.T) (*server, *fake.Clientset) {
	t.Helper()
	cfg := testConfig()
	client := fake.NewSimpleClientset()
	return &server{cfg: cfg, builds: &Builds{cfg: cfg, client: client}, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}, client
}

func TestAPICreateListGetDelete(t *testing.T) {
	s, client := newTestServer(t)
	h := s.routes()

	body := `{"os":"ubuntu-2204","kubernetesVersion":"v1.35.0","credentials":{"username":"a","password":"b"}}`
	req := httptest.NewRequest("POST", "/api/builds", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created Build
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == "" || created.ImageName != "nkp-ubuntu-22.04-release-1.35.0-20261105193022.qcow2" {
		t.Fatalf("created = %+v", created)
	}

	ctx := context.Background()
	cm, err := client.CoreV1().ConfigMaps("builds").Get(ctx, created.Name, metav1.GetOptions{})
	if err != nil || len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Kind != "Job" {
		t.Fatalf("configmap owner not set: %v %+v", err, cm)
	}
	sec, err := client.CoreV1().Secrets("builds").Get(ctx, created.Name+"-creds", metav1.GetOptions{})
	if err != nil || len(sec.OwnerReferences) != 1 {
		t.Fatalf("secret owner not set: %v", err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/builds", nil))
	var list []Build
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Request == nil || list[0].Request.Credentials.Password != "********" {
		t.Fatalf("list = %s", rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/builds/"+created.ID, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "kubernetes_series") {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/builds/"+created.ID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/builds/"+created.ID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", rec.Code)
	}
}

func TestAPIValidationAndCSRF(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.routes()

	req := httptest.NewRequest("POST", "/api/builds", strings.NewReader(`{"os":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"os"`) {
		t.Fatalf("validation: %d %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest("POST", "/api/builds", strings.NewReader(`os=ubuntu-2204`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form post should be rejected: %d", rec.Code)
	}

	req = httptest.NewRequest("DELETE", "/api/builds/deadbeef", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin delete should be rejected: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/builds/../../etc", nil))
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "root:") {
		t.Fatal("path traversal")
	}
}

func TestStaticAssetsServed(t *testing.T) {
	s, _ := newTestServer(t)
	for _, p := range []string{"/", "/app.js", "/style.css"} {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}
