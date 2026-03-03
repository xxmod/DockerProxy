package app

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultListenAddr       = ":8080"
	defaultPublicBaseURL    = "http://localhost:8080"
	defaultUpstreamRegistry = "https://registry-1.docker.io"
	defaultUpstreamAuth     = "https://auth.docker.io/token"
	defaultCacheDir         = "./data/cache"
	defaultConfigFile       = "./data/config.json"
)

var realmRegex = regexp.MustCompile(`realm="([^"]+)"`)

//go:embed index.html
var webFS embed.FS

type Config struct {
	ListenAddr          string        `json:"listen_addr"`
	ConfigFilePath      string        `json:"config_file_path"`
	EnableHTTPS         bool          `json:"enable_https"`
	TLSCertFile         string        `json:"tls_cert_file"`
	TLSKeyFile          string        `json:"tls_key_file"`
	PublicBaseURL       string        `json:"public_base_url"`
	UpstreamRegistry    string        `json:"upstream_registry"`
	UpstreamAuthRealm   string        `json:"upstream_auth_realm"`
	CacheDir            string        `json:"cache_dir"`
	CacheTTL            time.Duration `json:"cache_ttl"`
	CacheObjectMaxBytes int64         `json:"cache_object_max_bytes"`
	RequestTimeout      time.Duration `json:"request_timeout"`
	AdminToken          string        `json:"-"`
}

type configResponse struct {
	ListenAddr          string `json:"listen_addr"`
	ConfigFilePath      string `json:"config_file_path"`
	EnableHTTPS         bool   `json:"enable_https"`
	TLSCertFile         string `json:"tls_cert_file"`
	TLSKeyFile          string `json:"tls_key_file"`
	PublicBaseURL       string `json:"public_base_url"`
	UpstreamRegistry    string `json:"upstream_registry"`
	UpstreamAuthRealm   string `json:"upstream_auth_realm"`
	CacheDir            string `json:"cache_dir"`
	CacheTTL            string `json:"cache_ttl"`
	CacheObjectMaxBytes int64  `json:"cache_object_max_bytes"`
	RequestTimeout      string `json:"request_timeout"`
	AdminTokenEnabled   bool   `json:"admin_token_enabled"`
}

type configUpdateRequest struct {
	EnableHTTPS       *bool   `json:"enable_https"`
	TLSCertFile       *string `json:"tls_cert_file"`
	TLSKeyFile        *string `json:"tls_key_file"`
	PublicBaseURL     *string `json:"public_base_url"`
	UpstreamRegistry  *string `json:"upstream_registry"`
	UpstreamAuthRealm *string `json:"upstream_auth_realm"`
}

type persistedConfig struct {
	EnableHTTPS       bool   `json:"enable_https"`
	TLSCertFile       string `json:"tls_cert_file"`
	TLSKeyFile        string `json:"tls_key_file"`
	PublicBaseURL     string `json:"public_base_url"`
	UpstreamRegistry  string `json:"upstream_registry"`
	UpstreamAuthRealm string `json:"upstream_auth_realm"`
}

type metrics struct {
	totalRequests  atomic.Uint64
	cacheHits      atomic.Uint64
	cacheMisses    atomic.Uint64
	upstreamErrors atomic.Uint64
}

type cacheMeta struct {
	StatusCode int         `json:"status_code"`
	Header     http.Header `json:"header"`
	ExpireAt   time.Time   `json:"expire_at"`
}

type cacheEntry struct {
	Meta cacheMeta
	Body []byte
}

type manifestCache struct {
	dir            string
	ttl            time.Duration
	maxObjectBytes int64
	mu             sync.RWMutex
}

func newManifestCache(dir string, ttl time.Duration, maxObjectBytes int64) (*manifestCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &manifestCache{dir: dir, ttl: ttl, maxObjectBytes: maxObjectBytes}, nil
}

func (c *manifestCache) setPolicy(ttl time.Duration, maxObjectBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl = ttl
	c.maxObjectBytes = maxObjectBytes
}

func (c *manifestCache) key(req *http.Request) string {
	accept := req.Header.Get("Accept")
	raw := req.Method + "|" + req.URL.Path + "|" + req.URL.RawQuery + "|" + accept
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (c *manifestCache) get(key string) (*cacheEntry, bool, error) {
	metaPath := filepath.Join(c.dir, key+".meta.json")
	bodyPath := filepath.Join(c.dir, key+".body")

	metaRaw, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}

	var meta cacheMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, false, err
	}
	if time.Now().After(meta.ExpireAt) {
		_ = os.Remove(metaPath)
		_ = os.Remove(bodyPath)
		return nil, false, nil
	}

	body, err := os.ReadFile(bodyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}

	return &cacheEntry{Meta: meta, Body: body}, true, nil
}

func (c *manifestCache) set(key string, statusCode int, header http.Header, body []byte) error {
	c.mu.RLock()
	ttl := c.ttl
	maxObjectBytes := c.maxObjectBytes
	c.mu.RUnlock()

	if int64(len(body)) > maxObjectBytes {
		return nil
	}

	meta := cacheMeta{StatusCode: statusCode, Header: header.Clone(), ExpireAt: time.Now().Add(ttl)}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	metaPath := filepath.Join(c.dir, key+".meta.json")
	bodyPath := filepath.Join(c.dir, key+".body")
	if err := os.WriteFile(bodyPath, body, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(metaPath, metaRaw, 0o644); err != nil {
		return err
	}
	return nil
}

func (c *manifestCache) clear() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(c.dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

type Server struct {
	httpServer *http.Server
	client     *http.Client
	cache      *manifestCache
	metrics    *metrics
	cfgMu      sync.RWMutex
	cfg        Config
}

func LoadConfigFromEnv() Config {
	cacheTTL := 12 * time.Hour
	if v := strings.TrimSpace(os.Getenv("CACHE_TTL")); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil {
			cacheTTL = parsed
		}
	}

	requestTimeout := 60 * time.Second
	if v := strings.TrimSpace(os.Getenv("REQUEST_TIMEOUT")); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil {
			requestTimeout = parsed
		}
	}

	maxObjectBytes := int64(8 * 1024 * 1024)
	if v := strings.TrimSpace(os.Getenv("CACHE_OBJECT_MAX_BYTES")); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			maxObjectBytes = parsed
		}
	}

	cfg := Config{
		ListenAddr:          envOrDefault("LISTEN_ADDR", defaultListenAddr),
		ConfigFilePath:      envOrDefault("CONFIG_FILE", defaultConfigFile),
		EnableHTTPS:         parseEnvBool("ENABLE_HTTPS", false),
		TLSCertFile:         strings.TrimSpace(os.Getenv("TLS_CERT_FILE")),
		TLSKeyFile:          strings.TrimSpace(os.Getenv("TLS_KEY_FILE")),
		PublicBaseURL:       envOrDefault("PUBLIC_BASE_URL", defaultPublicBaseURL),
		UpstreamRegistry:    envOrDefault("UPSTREAM_REGISTRY", defaultUpstreamRegistry),
		UpstreamAuthRealm:   envOrDefault("UPSTREAM_AUTH_REALM", defaultUpstreamAuth),
		CacheDir:            envOrDefault("CACHE_DIR", defaultCacheDir),
		CacheTTL:            cacheTTL,
		CacheObjectMaxBytes: maxObjectBytes,
		RequestTimeout:      requestTimeout,
		AdminToken:          os.Getenv("ADMIN_TOKEN"),
	}
	if persisted, err := loadPersistedConfig(cfg.ConfigFilePath); err != nil {
		log.Printf("load persisted config failed: %v", err)
	} else if persisted != nil {
		cfg.EnableHTTPS = persisted.EnableHTTPS
		cfg.TLSCertFile = persisted.TLSCertFile
		cfg.TLSKeyFile = persisted.TLSKeyFile
		cfg.PublicBaseURL = persisted.PublicBaseURL
		cfg.UpstreamRegistry = persisted.UpstreamRegistry
		cfg.UpstreamAuthRealm = persisted.UpstreamAuthRealm
	}
	cfg.normalize()
	return cfg
}

func loadPersistedConfig(path string) (*persistedConfig, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var cfg persistedConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func savePersistedConfig(path string, cfg persistedConfig) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func parseEnvBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func (c *Config) normalize() {
	if c.ListenAddr == "" {
		c.ListenAddr = defaultListenAddr
	}
	if c.ConfigFilePath == "" {
		c.ConfigFilePath = defaultConfigFile
	}
	c.ConfigFilePath = strings.TrimSpace(c.ConfigFilePath)
	c.TLSCertFile = strings.TrimSpace(c.TLSCertFile)
	c.TLSKeyFile = strings.TrimSpace(c.TLSKeyFile)
	if !c.EnableHTTPS && c.TLSCertFile != "" && c.TLSKeyFile != "" {
		c.EnableHTTPS = true
	}
	if c.PublicBaseURL == "" {
		c.PublicBaseURL = defaultPublicBaseURL
	}
	if c.UpstreamRegistry == "" {
		c.UpstreamRegistry = defaultUpstreamRegistry
	}
	if c.UpstreamAuthRealm == "" {
		c.UpstreamAuthRealm = defaultUpstreamAuth
	}
	if c.CacheDir == "" {
		c.CacheDir = defaultCacheDir
	}
	if c.CacheTTL <= 0 {
		c.CacheTTL = 12 * time.Hour
	}
	if c.CacheObjectMaxBytes <= 0 {
		c.CacheObjectMaxBytes = 8 * 1024 * 1024
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60 * time.Second
	}
	if c.EnableHTTPS && strings.HasPrefix(strings.ToLower(c.PublicBaseURL), "http://") {
		c.PublicBaseURL = "https://" + strings.TrimPrefix(c.PublicBaseURL, "http://")
	}
	c.PublicBaseURL = strings.TrimRight(c.PublicBaseURL, "/")
	c.UpstreamRegistry = strings.TrimRight(c.UpstreamRegistry, "/")
}

func NewServer(cfg Config) (*Server, error) {
	cfg.normalize()
	cache, err := newManifestCache(cfg.CacheDir, cfg.CacheTTL, cfg.CacheObjectMaxBytes)
	if err != nil {
		return nil, err
	}

	s := &Server{
		client:  &http.Client{Timeout: cfg.RequestTimeout},
		cache:   cache,
		metrics: &metrics{},
		cfg:     cfg,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/admin/config", s.handleAdminConfig)
	mux.HandleFunc("/api/admin/stats", s.handleAdminStats)
	mux.HandleFunc("/api/admin/cache", s.handleAdminCache)
	mux.HandleFunc("/auth/token", s.handleAuthToken)
	mux.HandleFunc("/v2", s.handleV2)
	mux.HandleFunc("/v2/", s.handleV2)
	mux.HandleFunc("/", s.handleIndex)

	s.httpServer = &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      loggingMiddleware(mux),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	return s, nil
}

func (s *Server) getConfig() Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) updateConfig(update configUpdateRequest) (Config, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	if update.EnableHTTPS != nil {
		s.cfg.EnableHTTPS = *update.EnableHTTPS
	}
	if update.TLSCertFile != nil {
		s.cfg.TLSCertFile = strings.TrimSpace(*update.TLSCertFile)
	}
	if update.TLSKeyFile != nil {
		s.cfg.TLSKeyFile = strings.TrimSpace(*update.TLSKeyFile)
	}
	if update.PublicBaseURL != nil {
		s.cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(*update.PublicBaseURL), "/")
	}
	if update.UpstreamRegistry != nil {
		s.cfg.UpstreamRegistry = strings.TrimRight(strings.TrimSpace(*update.UpstreamRegistry), "/")
	}
	if update.UpstreamAuthRealm != nil {
		s.cfg.UpstreamAuthRealm = strings.TrimSpace(*update.UpstreamAuthRealm)
	}
	s.cfg.normalize()

	err := savePersistedConfig(s.cfg.ConfigFilePath, persistedConfig{
		EnableHTTPS:       s.cfg.EnableHTTPS,
		TLSCertFile:       s.cfg.TLSCertFile,
		TLSKeyFile:        s.cfg.TLSKeyFile,
		PublicBaseURL:     s.cfg.PublicBaseURL,
		UpstreamRegistry:  s.cfg.UpstreamRegistry,
		UpstreamAuthRealm: s.cfg.UpstreamAuthRealm,
	})
	if err != nil {
		return Config{}, err
	}

	return s.cfg, nil
}

func (s *Server) ListenAndServe() error {
	cfg := s.getConfig()
	var err error
	if cfg.EnableHTTPS {
		if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
			return errors.New("https enabled but TLS_CERT_FILE or TLS_KEY_FILE is empty")
		}
		err = s.httpServer.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	} else {
		err = s.httpServer.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	content, err := webFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "index not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(content)
}

func (s *Server) handleV2(w http.ResponseWriter, r *http.Request) {
	s.metrics.totalRequests.Add(1)

	cacheable := isCacheableRequest(r)
	cacheKey := ""
	if cacheable {
		cacheKey = s.cache.key(r)
		entry, ok, err := s.cache.get(cacheKey)
		if err != nil {
			log.Printf("cache get error: %v", err)
		}
		if ok {
			s.metrics.cacheHits.Add(1)
			copyHeader(w.Header(), entry.Meta.Header)
			w.WriteHeader(entry.Meta.StatusCode)
			_, _ = w.Write(entry.Body)
			return
		}
		s.metrics.cacheMisses.Add(1)
	}

	cfg := s.getConfig()
	targetURL := cfg.UpstreamRegistry + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "build upstream request failed", http.StatusInternalServerError)
		return
	}
	copyHeader(upstreamReq.Header, r.Header)
	upstreamReq.Host = mustHost(cfg.UpstreamRegistry)
	upstreamReq.Header.Del("Host")

	resp, err := s.client.Do(upstreamReq)
	if err != nil {
		s.metrics.upstreamErrors.Add(1)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	rewriteAuthHeader(resp.Header, cfg.PublicBaseURL)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if cacheable && resp.StatusCode == http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			http.Error(w, "read upstream body failed", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(body)
		if err := s.cache.set(cacheKey, resp.StatusCode, resp.Header, body); err != nil {
			log.Printf("cache set error: %v", err)
		}
		return
	}

	_, _ = io.Copy(w, resp.Body)
}

func isCacheableRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	path := r.URL.Path
	if strings.Contains(path, "/manifests/") {
		return true
	}
	return strings.HasSuffix(path, "/tags/list")
}

func rewriteAuthHeader(header http.Header, publicBaseURL string) {
	original := header.Get("Www-Authenticate")
	if original == "" {
		return
	}
	realmURL := publicBaseURL + "/auth/token"
	rewritten := realmRegex.ReplaceAllString(original, fmt.Sprintf(`realm="%s"`, realmURL))
	header.Set("Www-Authenticate", rewritten)
}

func (s *Server) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	cfg := s.getConfig()
	target, err := url.Parse(cfg.UpstreamAuthRealm)
	if err != nil {
		http.Error(w, "bad upstream auth url", http.StatusInternalServerError)
		return
	}
	target.RawQuery = r.URL.RawQuery

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
	if err != nil {
		http.Error(w, "build auth request failed", http.StatusInternalServerError)
		return
	}
	copyHeader(proxyReq.Header, r.Header)
	proxyReq.Header.Del("Host")

	resp, err := s.client.Do(proxyReq)
	if err != nil {
		http.Error(w, "auth upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query q is required"})
		return
	}
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}
	pageSize := r.URL.Query().Get("page_size")
	if pageSize == "" {
		pageSize = "20"
	}

	target := fmt.Sprintf("https://hub.docker.com/v2/search/repositories/?query=%s&page=%s&page_size=%s", url.QueryEscape(query), url.QueryEscape(page), url.QueryEscape(pageSize))
	searchReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build search request failed"})
		return
	}

	resp, err := s.client.Do(searchReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "docker hub unavailable"})
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.getConfig()
		writeJSON(w, http.StatusOK, toConfigResponse(cfg))
	case http.MethodPut:
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		var req configUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
			return
		}
		cfg, err := s.updateConfig(req)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist config failed"})
			return
		}
		writeJSON(w, http.StatusOK, toConfigResponse(cfg))
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total_requests":  s.metrics.totalRequests.Load(),
		"cache_hits":      s.metrics.cacheHits.Load(),
		"cache_misses":    s.metrics.cacheMisses.Load(),
		"upstream_errors": s.metrics.upstreamErrors.Load(),
		"cache_ttl":       s.getConfig().CacheTTL.String(),
		"cache_dir":       s.getConfig().CacheDir,
	})
}

func (s *Server) handleAdminCache(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		entries, err := os.ReadDir(s.getConfig().CacheDir)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read cache failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"entry_files": len(entries)})
	case http.MethodDelete:
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if err := s.cache.clear(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "clear cache failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "cache cleared"})
	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) authorized(r *http.Request) bool {
	cfg := s.getConfig()
	if cfg.AdminToken == "" {
		return true
	}
	token := strings.TrimSpace(r.Header.Get("X-Admin-Token"))
	if token != "" {
		return token == cfg.AdminToken
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:]) == cfg.AdminToken
	}
	return false
}

func toConfigResponse(cfg Config) configResponse {
	return configResponse{
		ListenAddr:          cfg.ListenAddr,
		ConfigFilePath:      cfg.ConfigFilePath,
		EnableHTTPS:         cfg.EnableHTTPS,
		TLSCertFile:         cfg.TLSCertFile,
		TLSKeyFile:          cfg.TLSKeyFile,
		PublicBaseURL:       cfg.PublicBaseURL,
		UpstreamRegistry:    cfg.UpstreamRegistry,
		UpstreamAuthRealm:   cfg.UpstreamAuthRealm,
		CacheDir:            cfg.CacheDir,
		CacheTTL:            cfg.CacheTTL.String(),
		CacheObjectMaxBytes: cfg.CacheObjectMaxBytes,
		RequestTimeout:      cfg.RequestTimeout.String(),
		AdminTokenEnabled:   cfg.AdminToken != "",
	}
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func mustHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
