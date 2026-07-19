package releasesmoke

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// ReleaseAssets is a downloaded/staged GitHub release asset set for one tag.
type ReleaseAssets struct {
	Tag          string
	PlainVersion string
	Asset        string
	ArchivePath  string
	SumsPath     string
	SignaturePath string
}

// MockReleaseAPI serves a local GitHub-like release API for install.sh / upgrade.
type MockReleaseAPI struct {
	URL      string
	server   *http.Server
	listener net.Listener
	repo     string
	tag      string
	assets   map[string]string
	wg       sync.WaitGroup
}

// StartMockReleaseAPI starts a loopback HTTP server serving release metadata and assets.
func StartMockReleaseAPI(repo string, release ReleaseAssets) (*MockReleaseAPI, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	baseURL := "http://" + ln.Addr().String()
	assets := map[string]string{
		release.Asset:           release.ArchivePath,
		"SHA256SUMS":            release.SumsPath,
		"SHA256SUMS.sigstore":   release.SignaturePath,
	}
	api := &MockReleaseAPI{
		URL:      baseURL,
		listener: ln,
		repo:     repo,
		tag:      release.Tag,
		assets:   assets,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/__health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/__stop", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "stopping"})
		go func() { _ = api.server.Close() }()
	})
	mux.HandleFunc("/", api.serve)
	api.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	api.wg.Add(1)
	go func() {
		defer api.wg.Done()
		_ = api.server.Serve(ln)
	}()
	// wait for health
	for i := 0; i < 50; i++ {
		resp, err := http.Get(baseURL + "/__health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return api, nil
			}
		}
	}
	_ = api.Close()
	return nil, fmt.Errorf("local release API did not become ready at %s", baseURL)
}

// Close stops the mock API.
func (m *MockReleaseAPI) Close() error {
	if m == nil || m.server == nil {
		return nil
	}
	err := m.server.Close()
	m.wg.Wait()
	return err
}

func (m *MockReleaseAPI) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"message": "method not allowed"})
		return
	}
	reqPath, err := url.PathUnescape(r.URL.Path)
	if err != nil {
		writeJSON(w, 400, map[string]string{"message": "bad path"})
		return
	}
	releasePath := "/repos/" + m.repo + "/releases/tags/" + m.tag
	if reqPath == releasePath {
		assetPayload := make([]map[string]string, 0, len(m.assets))
		for name := range m.assets {
			assetPayload = append(assetPayload, map[string]string{
				"name":                 name,
				"browser_download_url": m.URL + "/assets/" + url.PathEscape(name),
			})
		}
		writeJSON(w, 200, map[string]any{
			"tag_name":   m.tag,
			"draft":      false,
			"prerelease": false,
			"assets":     assetPayload,
		})
		return
	}
	downloadPrefix := "/" + m.repo + "/releases/download/" + m.tag + "/"
	if strings.HasPrefix(reqPath, downloadPrefix) {
		name := strings.TrimPrefix(reqPath, downloadPrefix)
		m.serveAsset(w, name)
		return
	}
	if strings.HasPrefix(reqPath, "/assets/") {
		name := path.Base(reqPath)
		// path.Base may decode; also try raw
		if unesc, err := url.PathUnescape(strings.TrimPrefix(reqPath, "/assets/")); err == nil {
			name = unesc
		}
		m.serveAsset(w, name)
		return
	}
	writeJSON(w, 404, map[string]string{"message": "not found"})
}

func (m *MockReleaseAPI) serveAsset(w http.ResponseWriter, name string) {
	p, ok := m.assets[name]
	if !ok {
		writeJSON(w, 404, map[string]string{"message": "not found"})
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeJSON(w, 500, map[string]string{"message": err.Error()})
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeJSON(w, 500, map[string]string{"message": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.WriteHeader(200)
	_, _ = io.Copy(w, f)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	b, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
