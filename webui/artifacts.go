package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The artifact store holds what air-gapped builds install from: the files of
// an NKP release's image-artifacts directory (containerd-*.tar.gz and
// images/kubernetes-images-*.tar) and OS package bundles, either uploaded
// through the UI or saved there by harvesting builds. It is a directory on
// the UI pod's PersistentVolume. Build pods download what they need from it
// with the artifacts token (see airgapPreflight and buildObjects).

// Artifact kinds, by file name.
const (
	artifactOSPackages = "os-packages"
	artifactContainerd = "containerd"
	artifactImages     = "images"
	artifactOther      = "other"
)

var (
	// A plain file name, or one under images/ (NKP's layout for image
	// bundles). No other directories, so no path traversal.
	reArtifactName = regexp.MustCompile(`^(images/)?[A-Za-z0-9][A-Za-z0-9._+-]{0,199}$`)
	// NKP's containerd bundles.
	reContainerdArtifact = regexp.MustCompile(`^containerd-[0-9.]+-d2iq\.1-[a-z]+-[0-9.]+-x86_64(_fips)?\.tar\.gz$`)
	// OS package bundles from `nkp create package-bundle` and harvesting
	// builds: <k8s>_<os>_<os version>_x86_64[_fips][_stig][_gpu].tar.gz.
	reOSPackagesArtifact = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+_[a-z]+_[0-9.]+_x86_64(_[a-z0-9]+)*\.tar\.gz$`)
)

func artifactKind(name string) string {
	switch {
	case strings.HasPrefix(name, "images/"):
		return artifactImages
	case reContainerdArtifact.MatchString(name):
		return artifactContainerd
	case reOSPackagesArtifact.MatchString(name):
		return artifactOSPackages
	default:
		return artifactOther
	}
}

type Artifact struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	Modified time.Time `json:"modified"`
}

type artifactList struct {
	Artifacts []Artifact `json:"artifacts"`
	// Free space on the store's volume, for the upload form.
	FreeBytes uint64 `json:"freeBytes"`
}

type artifactStore struct {
	dir string
}

var errBadArtifactName = errors.New("artifact names are a file name, or images/<file name>: letters, digits, '.', '_', '+' or '-'")

func (s *artifactStore) path(name string) (string, error) {
	if !reArtifactName.MatchString(name) || strings.Contains(name, "..") {
		return "", errBadArtifactName
	}
	return filepath.Join(s.dir, filepath.FromSlash(name)), nil
}

// The sha256 of each artifact is kept next to it, hidden, when it's stored.
func checksumPath(p string) string {
	return filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+".sha256")
}

func (s *artifactStore) stat(name string) (Artifact, error) {
	p, err := s.path(name)
	if err != nil {
		return Artifact{}, err
	}
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.Mode().IsRegular()) {
		return Artifact{}, errNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	sum, _ := os.ReadFile(checksumPath(p))
	return Artifact{Name: name, Kind: artifactKind(name), Size: fi.Size(),
		SHA256: strings.TrimSpace(string(sum)), Modified: fi.ModTime().UTC()}, nil
}

func (s *artifactStore) list() (artifactList, error) {
	out := artifactList{Artifacts: []Artifact{}}
	for _, sub := range []string{"", "images"} {
		entries, err := os.ReadDir(filepath.Join(s.dir, sub))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, err
		}
		for _, e := range entries {
			name := path.Join(sub, e.Name())
			if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") || !reArtifactName.MatchString(name) {
				continue
			}
			a, err := s.stat(name)
			if err != nil {
				continue // removed meanwhile
			}
			out.Artifacts = append(out.Artifacts, a)
		}
	}
	sort.Slice(out.Artifacts, func(i, j int) bool { return out.Artifacts[i].Name < out.Artifacts[j].Name })
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &st); err == nil {
		out.FreeBytes = uint64(st.Bavail) * uint64(st.Bsize) //nolint:unconvert // Bsize's type differs by OS
	}
	return out, nil
}

// put stores r as name, replacing any artifact of that name only once it is
// complete, and records its sha256. A wantSHA256 that doesn't match fails it.
func (s *artifactStore) put(name string, r io.Reader, wantSHA256 string) (Artifact, error) {
	p, err := s.path(name)
	if err != nil {
		return Artifact{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return Artifact{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return Artifact{}, err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		return Artifact{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Artifact{}, err
	}
	if err := tmp.Close(); err != nil {
		return Artifact{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSHA256 != "" && !strings.EqualFold(wantSHA256, sum) {
		return Artifact{}, fmt.Errorf("%w: got sha256 %s, expected %s", errChecksumMismatch, sum, strings.ToLower(wantSHA256))
	}
	if err := os.WriteFile(checksumPath(p), []byte(sum+"\n"), 0o644); err != nil {
		return Artifact{}, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return Artifact{}, err
	}
	return s.stat(name)
}

var errChecksumMismatch = errors.New("upload corrupted")

func (s *artifactStore) delete(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); errors.Is(err, fs.ErrNotExist) {
		return errNotFound
	} else if err != nil {
		return err
	}
	_ = os.Remove(checksumPath(p))
	return nil
}

/* ---------- HTTP ---------- */

// artifactName is the {name...} path value, URL-decoded by the mux.
func (s *server) artifactName(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.artifacts == nil {
		writeError(w, http.StatusNotFound, "air-gapped builds are not enabled on this service", nil)
		return "", false
	}
	name := r.PathValue("name")
	if name != "" && !reArtifactName.MatchString(name) {
		writeError(w, http.StatusBadRequest, errBadArtifactName.Error(), nil)
		return "", false
	}
	return name, true
}

func (s *server) handleArtifactList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.artifactName(w, r); !ok {
		return
	}
	l, err := s.artifacts.list()
	if s.handleErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *server) handleArtifactGet(w http.ResponseWriter, r *http.Request) {
	name, ok := s.artifactName(w, r)
	if !ok {
		return
	}
	a, err := s.artifacts.stat(name)
	if s.handleErr(w, err) {
		return
	}
	p, _ := s.artifacts.path(name)
	f, err := os.Open(p)
	if s.handleErr(w, err) {
		return
	}
	defer f.Close()
	if a.SHA256 != "" {
		w.Header().Set("X-Checksum-Sha256", a.SHA256)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(name)))
	http.ServeContent(w, r, "", a.Modified, f)
}

// handleArtifactPut stores the raw request body. X-Checksum-Sha256, when
// sent, must match it.
func (s *server) handleArtifactPut(w http.ResponseWriter, r *http.Request) {
	name, ok := s.artifactName(w, r)
	if !ok {
		return
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "artifact name missing", nil)
		return
	}
	want := strings.TrimSpace(r.Header.Get("X-Checksum-Sha256"))
	if want != "" && !reSHA256.MatchString(strings.ToLower(want)) {
		writeError(w, http.StatusBadRequest, "X-Checksum-Sha256 must be a hex sha256", nil)
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.cfg.Airgap.MaxUploadBytes)
	a, err := s.artifacts.put(name, body, want)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("artifacts are limited to %d bytes (airgap.maxUploadBytes)", s.cfg.Airgap.MaxUploadBytes), nil)
		return
	case errors.Is(err, errChecksumMismatch), errors.Is(err, errBadArtifactName):
		writeError(w, http.StatusBadRequest, err.Error(), nil)
		return
	case s.handleErr(w, err):
		return
	}
	s.log.Info("artifact stored", "name", a.Name, "kind", a.Kind, "size", a.Size, "sha256", a.SHA256, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusCreated, a)
}

func (s *server) handleArtifactDelete(w http.ResponseWriter, r *http.Request) {
	name, ok := s.artifactName(w, r)
	if !ok {
		return
	}
	if s.handleErr(w, s.artifacts.delete(name)) {
		return
	}
	s.log.Info("artifact deleted", "name", name, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

// artifactTokenOK: build pods authenticate to the artifact store with the
// artifacts token instead of the UI's basic auth.
func artifactTokenOK(r *http.Request, token string) bool {
	if token == "" || !strings.HasPrefix(r.URL.Path, "/api/artifacts/") {
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
