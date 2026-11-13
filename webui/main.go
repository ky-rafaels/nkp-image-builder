package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed web
var webFS embed.FS

var reBuildID = regexp.MustCompile(`^[0-9a-f]{8}$`)

type server struct {
	cfg       *Config
	builds    *Builds
	log       *slog.Logger
	fipsCheck fipsRepoChecker
	// gpus lists the build cluster's GPUs from Prism Central; nil when GPU
	// discovery is off or has no credentials.
	gpus gpuLister
}

func main() {
	var (
		addr       = flag.String("addr", ":8080", "listen address")
		configPath = flag.String("config", envOr("CONFIG_FILE", "/etc/nkp-image-builder/config.json"), "path to config JSON")
		kubeconfig = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig for out-of-cluster development")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Error("loading config", "err", err)
		os.Exit(1)
	}
	restCfg, err := restConfig(*kubeconfig)
	if err != nil {
		log.Error("building kubernetes client config", "err", err)
		os.Exit(1)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Error("creating kubernetes client", "err", err)
		os.Exit(1)
	}

	s := &server{cfg: cfg, builds: &Builds{cfg: cfg, client: client}, log: log, fipsCheck: checkFIPSRepo}
	if cfg.GPU.Enabled && cfg.GPU.Discovery {
		if pc, err := newPrismClientFromEnv(); err != nil {
			log.Warn("GPU discovery off: users type the GPU name instead", "err", err)
		} else {
			s.gpus = pc
		}
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", *addr, "namespace", cfg.Namespace, "builderImage", cfg.Builder.Image)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/gpus", s.handleGPUs)
	mux.HandleFunc("GET /api/builds", s.handleList)
	mux.HandleFunc("POST /api/builds", s.handleCreate)
	mux.HandleFunc("GET /api/builds/{id}", s.handleGet)
	mux.HandleFunc("DELETE /api/builds/{id}", s.handleDelete)
	mux.HandleFunc("GET /api/builds/{id}/logs", s.handleLogs)

	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))

	return s.withMiddleware(mux)
}

// withMiddleware adds security headers, optional basic auth, and a same-origin
// check for state-changing requests.
func (s *server) withMiddleware(next http.Handler) http.Handler {
	user, pass := os.Getenv("BASIC_AUTH_USERNAME"), os.Getenv("BASIC_AUTH_PASSWORD")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:")

		if user != "" && r.URL.Path != "/healthz" {
			u, p, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				h.Set("WWW-Authenticate", `Basic realm="NKP Image Builder"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Browsers can't send application/json cross-site without a CORS
			// preflight (which we never answer), so this blocks CSRF. DELETE
			// has no body but still triggers a preflight cross-origin.
			if r.Method == http.MethodPost {
				ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if ct != "application/json" {
					writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json", nil)
					return
				}
			}
			if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r) {
				writeError(w, http.StatusForbidden, "cross-origin request rejected", nil)
				return
			}
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasSuffix(r.URL.Path, "/logs") {
			s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
		}
	})
}

func sameOrigin(origin string, r *http.Request) bool {
	o := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return o == host
}

type configResponse struct {
	Namespace     string        `json:"namespace"`
	BuilderImage  string        `json:"builderImage"`
	PrismEndpoint string        `json:"prismEndpoint"`
	Defaults      BuildDefaults `json:"defaults"`
	Catalog       CatalogConfig `json:"catalog"`
	Features      FeatureConfig `json:"features"`
	FIPS          fipsInfo      `json:"fips"`
	STIG          stigInfo      `json:"stig"`
	GPU           gpuInfo       `json:"gpu"`
}

type stigInfo struct {
	Enabled bool `json:"enabled"`
}

// gpuInfo is the part of GPUConfig the UI needs; driver URLs stay private.
type gpuInfo struct {
	Enabled bool `json:"enabled"`
	// Discovery: the form can list the cluster's GPUs from Prism Central.
	Discovery bool `json:"discovery"`
	// VGPUDriver says which vGPU guest driver packages are configured
	// (run, rpm, deb): the form only offers vGPU profiles for an OS that has
	// one, and only the rpm for DISA STIG builds.
	VGPUDriver   map[string]bool `json:"vgpuDriver"`
	DriverBranch string          `json:"driverBranch"`
}

// fipsInfo is the part of FIPSConfig the UI needs; source URLs stay private.
type fipsInfo struct {
	Enabled         bool   `json:"enabled"`
	BuildMetadata   string `json:"buildMetadata"`
	ImageRepository string `json:"imageRepository"`
}

func (s *server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, configResponse{
		Namespace:     s.cfg.Namespace,
		BuilderImage:  s.cfg.Builder.Image,
		PrismEndpoint: s.cfg.Nutanix.Endpoint,
		Defaults:      s.cfg.Defaults,
		Catalog:       s.cfg.Catalog,
		Features:      s.cfg.Features,
		FIPS: fipsInfo{
			Enabled:         s.cfg.FIPS.Enabled,
			BuildMetadata:   s.cfg.FIPS.BuildMetadata(),
			ImageRepository: s.cfg.FIPS.ImageRepository(),
		},
		STIG: stigInfo{Enabled: s.cfg.STIG.Enabled},
		GPU: gpuInfo{
			Enabled:   s.cfg.GPU.Enabled,
			Discovery: s.discoveryEnabled(),
			VGPUDriver: map[string]bool{
				"run": s.cfg.GPU.NVIDIA.VGPUDriver.Run.URL != "",
				"rpm": s.cfg.GPU.NVIDIA.VGPUDriver.RPM.URL != "",
				"deb": s.cfg.GPU.NVIDIA.VGPUDriver.DEB.URL != "",
			},
			DriverBranch: s.cfg.GPU.NVIDIA.Branch(),
		},
	})
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	builds, err := s.builds.List(r.Context())
	if err != nil {
		s.log.Error("listing builds", "err", err)
		writeError(w, http.StatusInternalServerError, "listing builds failed: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, builds)
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req BuildRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), nil)
		return
	}
	if err := req.normalize(s.cfg); err != nil {
		var fe fieldErrors
		if errors.As(err, &fe) {
			writeError(w, http.StatusUnprocessableEntity, "validation failed", fe)
			return
		}
		writeError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	fe := s.fipsPreflight(r.Context(), &req)
	for k, v := range s.gpuPreflight(r.Context(), &req) {
		if fe == nil {
			fe = fieldErrors{}
		}
		fe[k] = v
	}
	if fe != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation failed", fe)
		return
	}
	b, err := s.builds.Create(r.Context(), &req)
	if err != nil {
		s.log.Error("creating build", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	gpu := ""
	if req.GPU != nil {
		gpu = req.GPU.Name + " (" + req.GPU.Mode + ")"
	}
	s.log.Info("build created", "id", b.ID, "os", req.OS, "kubernetes", req.KubernetesVersion,
		"image", req.ImageName, "cluster", req.ClusterName, "gpu", gpu, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusCreated, b)
}

func (s *server) buildID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !reBuildID.MatchString(id) {
		writeError(w, http.StatusNotFound, errNotFound.Error(), nil)
		return "", false
	}
	return id, true
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.buildID(w, r)
	if !ok {
		return
	}
	b, err := s.builds.Get(r.Context(), id)
	if s.handleErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.buildID(w, r)
	if !ok {
		return
	}
	if s.handleErr(w, s.builds.Delete(r.Context(), id)) {
		return
	}
	s.log.Info("build deleted", "id", id, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := s.buildID(w, r)
	if !ok {
		return
	}
	follow := r.URL.Query().Get("follow") == "1"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx ingress buffering
	fw := &flushWriter{w: w, rc: http.NewResponseController(w)}
	err := s.builds.Logs(r.Context(), id, follow, fw)
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if !fw.wrote {
		s.handleErr(w, err)
		return
	}
	_, _ = fw.Write([]byte("\n[webui] log stream ended: " + err.Error() + "\n"))
}

func (s *server) handleErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, err.Error(), nil)
	default:
		s.log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error(), nil)
	}
	return true
}

type flushWriter struct {
	w     http.ResponseWriter
	rc    *http.ResponseController
	wrote bool
}

func (f *flushWriter) Write(p []byte) (int, error) {
	f.wrote = true
	n, err := f.w.Write(p)
	_ = f.rc.Flush()
	return n, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string, fields map[string]string) {
	writeJSON(w, status, map[string]any{"error": msg, "fields": fields})
}
