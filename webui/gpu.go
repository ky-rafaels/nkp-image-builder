package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ClusterGPU is one kind of GPU on a cluster: a passthrough GPU model or a
// vGPU profile, as the Nutanix Packer plugin matches it by name.
type ClusterGPU struct {
	Name   string `json:"name"`
	Vendor string `json:"vendor"`
	// Mode is "passthrough" or "vgpu"; PrismMode is what Prism Central
	// reports (PASSTHROUGH_COMPUTE, PASSTHROUGH_GRAPHICS, VIRTUAL).
	Mode      string `json:"mode"`
	PrismMode string `json:"prismMode"`
	// Total and Available count the cluster's GPUs (or vGPU slots) of this
	// kind, and how many Prism Central reports as assignable now.
	Total     int `json:"total"`
	Available int `json:"available"`
}

type ClusterGPUs struct {
	Cluster string `json:"cluster"`
	// ClusterFound is false when no host of that cluster is registered in
	// Prism Central (a typo, or a cluster managed elsewhere).
	ClusterFound bool         `json:"clusterFound"`
	GPUs         []ClusterGPU `json:"gpus"`
}

// find returns the GPU kind named name (case-insensitively, as the plugin
// matches), preferring one with a GPU free.
func (c *ClusterGPUs) find(name string) *ClusterGPU {
	var match *ClusterGPU
	for i := range c.GPUs {
		g := &c.GPUs[i]
		if strings.EqualFold(g.Name, name) && (match == nil || g.Available > match.Available) {
			match = g
		}
	}
	return match
}

type gpuLister interface {
	ClusterGPUs(ctx context.Context, cluster string) (*ClusterGPUs, error)
}

// prismClient lists GPUs with the Prism Central v3 hosts API. Every
// GPU-capable host reports its GPUs (and vGPU profiles) in gpu_list, with the
// same names the Packer plugin looks GPUs up by.
type prismClient struct {
	baseURL  string
	username string
	password string
	http     *http.Client
	ttl      time.Duration

	mu       sync.Mutex
	hosts    []v3Host
	cachedAt time.Time
}

// newPrismClientFromEnv uses the same NUTANIX_* variables as the builds.
func newPrismClientFromEnv() (*prismClient, error) {
	endpoint := strings.TrimSpace(os.Getenv("NUTANIX_ENDPOINT"))
	user, pass := os.Getenv("NUTANIX_USERNAME"), os.Getenv("NUTANIX_PASSWORD")
	if endpoint == "" || user == "" || pass == "" {
		return nil, errors.New("NUTANIX_ENDPOINT, NUTANIX_USERNAME and NUTANIX_PASSWORD must be set")
	}
	if !reHost.MatchString(endpoint) {
		return nil, fmt.Errorf("NUTANIX_ENDPOINT must be a hostname or IP without a scheme, not %q", endpoint)
	}
	port := strings.TrimSpace(os.Getenv("NUTANIX_PORT"))
	if port == "" {
		port = "9440"
	}
	if !rePort.MatchString(port) {
		return nil, fmt.Errorf("NUTANIX_PORT must be a port number, not %q", port)
	}
	insecure := strings.EqualFold(strings.TrimSpace(os.Getenv("NUTANIX_INSECURE")), "true")
	tr := http.DefaultTransport.(*http.Transport).Clone() // keeps HTTPS_PROXY/NO_PROXY
	// NUTANIX_INSECURE is the operator's choice, as for the builds.
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: insecure, MinVersion: tls.VersionTLS12}
	host := endpoint
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]" // IPv6 literal
	}
	return &prismClient{
		baseURL:  "https://" + host + ":" + port,
		username: user,
		password: pass,
		http:     &http.Client{Timeout: 20 * time.Second, Transport: tr},
		ttl:      30 * time.Second,
	}, nil
}

type v3Host struct {
	Status struct {
		Name             string `json:"name"`
		ClusterReference *struct {
			Name string `json:"name"`
			UUID string `json:"uuid"`
		} `json:"cluster_reference"`
		Resources struct {
			GPUList []v3GPU `json:"gpu_list"`
		} `json:"resources"`
	} `json:"status"`
}

type v3GPU struct {
	Name       string `json:"name"`
	Vendor     string `json:"vendor"`
	Mode       string `json:"mode"`
	Status     string `json:"status"`
	Assignable bool   `json:"assignable"`
}

type v3HostList struct {
	Entities []v3Host `json:"entities"`
	Metadata struct {
		TotalMatches int `json:"total_matches"`
	} `json:"metadata"`
}

func (p *prismClient) ClusterGPUs(ctx context.Context, cluster string) (*ClusterGPUs, error) {
	hosts, err := p.listHosts(ctx)
	if err != nil {
		return nil, err
	}
	return aggregateGPUs(hosts, cluster), nil
}

// listHosts returns every host Prism Central knows, cached briefly so the
// form and the create preflight don't each page through the API.
func (p *prismClient) listHosts(ctx context.Context) ([]v3Host, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hosts != nil && time.Since(p.cachedAt) < p.ttl {
		return p.hosts, nil
	}
	const pageSize, maxPages = 100, 50
	var all []v3Host
	for page := 0; page < maxPages; page++ {
		var resp v3HostList
		body := map[string]any{"kind": "host", "length": pageSize, "offset": len(all)}
		if err := p.post(ctx, "/api/nutanix/v3/hosts/list", body, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Entities...)
		if len(resp.Entities) == 0 || len(all) >= resp.Metadata.TotalMatches {
			break
		}
	}
	p.hosts, p.cachedAt = all, time.Now()
	return all, nil
}

func (p *prismClient) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.SetBasicAuth(p.username, p.password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err // drop the URL; it's the configured endpoint
		}
		return fmt.Errorf("contacting Prism Central: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("Prism Central rejected the credentials (%s)", resp.Status)
	case resp.StatusCode != http.StatusOK:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("Prism Central %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil {
		return fmt.Errorf("reading Prism Central's host list: %w", err)
	}
	return nil
}

// aggregateGPUs groups a cluster's GPUs by name and mode.
func aggregateGPUs(hosts []v3Host, cluster string) *ClusterGPUs {
	out := &ClusterGPUs{Cluster: cluster, GPUs: []ClusterGPU{}}
	byKey := map[string]*ClusterGPU{}
	for _, h := range hosts {
		ref := h.Status.ClusterReference
		if ref == nil || !(strings.EqualFold(ref.Name, cluster) || strings.EqualFold(ref.UUID, cluster)) {
			continue
		}
		out.ClusterFound = true
		for _, g := range h.Status.Resources.GPUList {
			if g.Name == "" {
				continue
			}
			mode := gpuModePassthrough
			if strings.Contains(strings.ToUpper(g.Mode), "VIRTUAL") {
				mode = gpuModeVGPU
			}
			key := strings.ToLower(g.Name) + "\x00" + mode
			c := byKey[key]
			if c == nil {
				c = &ClusterGPU{Name: g.Name, Vendor: gpuVendor(g.Vendor), Mode: mode, PrismMode: g.Mode}
				byKey[key] = c
			}
			c.Total++
			if g.Assignable {
				c.Available++
			}
		}
	}
	for _, c := range byKey {
		out.GPUs = append(out.GPUs, *c)
	}
	sort.Slice(out.GPUs, func(i, j int) bool {
		a, b := out.GPUs[i], out.GPUs[j]
		if a.Mode != b.Mode {
			return a.Mode == gpuModePassthrough
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out
}

// gpuVendor tidies Prism's vendor strings ("kNvidia", "NVIDIA", ...).
func gpuVendor(v string) string {
	switch l := strings.ToLower(v); {
	case strings.Contains(l, "nvidia"):
		return "NVIDIA"
	case strings.Contains(l, "amd"):
		return "AMD"
	case strings.Contains(l, "intel"):
		return "Intel"
	default:
		return v
	}
}

// discoveryEnabled reports whether the form can list GPUs from Prism Central.
func (s *server) discoveryEnabled() bool {
	return s.cfg.GPU.Enabled && s.cfg.GPU.Discovery && s.gpus != nil
}

func (s *server) handleGPUs(w http.ResponseWriter, r *http.Request) {
	switch {
	case !s.cfg.GPU.Enabled || !s.cfg.GPU.Discovery:
		writeError(w, http.StatusNotFound, "GPU discovery is not enabled on this service", nil)
		return
	case s.gpus == nil:
		writeError(w, http.StatusServiceUnavailable,
			"GPU discovery needs Prism Central credentials (NUTANIX_ENDPOINT, NUTANIX_USERNAME, NUTANIX_PASSWORD) in the UI pod", nil)
		return
	}
	cluster := strings.TrimSpace(r.URL.Query().Get("cluster"))
	if !reNutanixRef.MatchString(cluster) {
		writeError(w, http.StatusBadRequest, "cluster is required", nil)
		return
	}
	list, err := s.gpus.ClusterGPUs(r.Context(), cluster)
	if err != nil {
		s.log.Warn("listing GPUs", "cluster", cluster, "err", err)
		writeError(w, http.StatusBadGateway, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// gpuPreflight checks the requested GPU against Prism Central before a build
// starts: it must be an NVIDIA GPU on the build cluster, of the requested
// type, with one free. A Prism Central error doesn't block the build (the
// plugin reports a missing GPU itself); a definite "not there" does.
func (s *server) gpuPreflight(ctx context.Context, req *BuildRequest) fieldErrors {
	g := req.GPU
	if g == nil || !s.discoveryEnabled() {
		return nil
	}
	// Per-build credentials may point at a different Prism Central.
	if c := req.Credentials; !c.empty() && (c.Endpoint != "" || c.Username != "") {
		return nil
	}
	list, err := s.gpus.ClusterGPUs(ctx, req.ClusterName)
	if err != nil {
		s.log.Warn("GPU preflight inconclusive", "cluster", req.ClusterName, "err", err)
		return nil
	}
	found := list.find(g.Name)
	switch {
	case !list.ClusterFound:
		return fieldErrors{"clusterName": fmt.Sprintf("no cluster named %q in Prism Central", req.ClusterName)}
	case found == nil:
		names := make([]string, 0, len(list.GPUs))
		for _, c := range list.GPUs {
			names = append(names, c.Name)
		}
		have := "it has no GPUs"
		if len(names) > 0 {
			have = "it has: " + strings.Join(names, ", ")
		}
		return fieldErrors{"gpu": fmt.Sprintf("cluster %s has no GPU named %q; %s", list.Cluster, g.Name, have)}
	case found.Vendor != "NVIDIA":
		return fieldErrors{"gpu": fmt.Sprintf("%s is a %s GPU; only NVIDIA GPUs are supported", found.Name, found.Vendor)}
	case found.Mode != g.Mode:
		return fieldErrors{"gpu": fmt.Sprintf("%s is %s on cluster %s, not %s", found.Name, gpuKind(found.Mode), list.Cluster, gpuKind(g.Mode))}
	case found.Available == 0:
		return fieldErrors{"gpu": fmt.Sprintf("all %d of %s on cluster %s are in use", found.Total, found.Name, list.Cluster)}
	}
	return nil
}

func gpuKind(mode string) string {
	if mode == gpuModeVGPU {
		return "a vGPU profile"
	}
	return "a passthrough GPU"
}
