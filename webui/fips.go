package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// fipsRepoChecker reports whether a yum repo exists. found=false with a nil
// error means the server definitely said no.
type fipsRepoChecker func(ctx context.Context, repoURL string) (found bool, err error)

var fipsHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	// http.DefaultTransport honours HTTP(S)_PROXY / NO_PROXY.
	Transport: http.DefaultTransport,
}

// checkFIPSRepo HEADs <repo>/repodata/repomd.xml. Object stores answer 403
// rather than 404 for missing keys, so both count as "not found".
func checkFIPSRepo(ctx context.Context, repoURL string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, repoURL+"/repodata/repomd.xml", nil)
	if err != nil {
		return false, err
	}
	resp, err := fipsHTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected status %s", resp.Status)
	}
}

// fipsPreflight returns field errors when a FIPS build can't possibly succeed.
func (s *server) fipsPreflight(ctx context.Context, req *BuildRequest) fieldErrors {
	if !req.FIPS || !s.cfg.FIPS.Preflight {
		return nil
	}
	repo := s.cfg.FIPS.RPMRepo(req.KubernetesVersion)
	found, err := s.fipsCheck(ctx, repo)
	if err != nil {
		// Don't block builds on the UI pod's own egress; the build will
		// report a missing repo itself.
		s.log.Warn("FIPS repo preflight inconclusive", "repo", repo, "err", err)
		return nil
	}
	if !found {
		return fieldErrors{"kubernetesVersion": fmt.Sprintf(
			"no FIPS Kubernetes packages for %s (%s); pick a version with a FIPS release", req.KubernetesVersion, repo)}
	}
	return nil
}
