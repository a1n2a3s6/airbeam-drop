package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/a1n2a3s6/airbeam-drop/internal/qr"
	"github.com/a1n2a3s6/airbeam-drop/internal/server/web"
)

const (
	authCookieName      = "airbeam_session"
	maxClipboardEntries = 50
	maxClipboardBytes   = 1 << 20 // 1 MB
	copyBufferSize      = 1 << 20 // 1 MB streaming buffer for multi-GB uploads
)

// Config configures the AirBeam HTTP server.
type Config struct {
	Host    string
	Port    int
	SaveDir string
	PIN     string // Empty string disables PIN authentication (--no-pin).
	Logger  func(format string, args ...any)
}

// FileEntry represents a shared or uploaded file available for download.
type FileEntry struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	SizeHuman  string    `json:"sizeHuman"`
	Ext        string    `json:"ext"`
	MimeType   string    `json:"mimeType"`
	Source     string    `json:"source"` // "pc" or "phone"
	Device     string    `json:"device"`
	UploadedAt time.Time `json:"uploadedAt"`
	DiskPath   string    `json:"-"`
}

// ClipboardEntry represents a beamed text or link item.
type ClipboardEntry struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Device    string    `json:"device"`
	CreatedAt time.Time `json:"createdAt"`
}

// Server holds AirBeam's HTTP router, file registry, clipboard history, and SSE hub.
type Server struct {
	cfg          Config
	hub          *Hub
	sessionToken string
	mux          *http.ServeMux
	staticFS     fs.FS

	mu        sync.RWMutex
	files     []FileEntry
	fileIndex map[string]FileEntry
	clipboard []ClipboardEntry
}

// New constructs a new AirBeam Server instance.
func New(cfg Config) (*Server, error) {
	if cfg.SaveDir == "" {
		return nil, errors.New("save directory must not be empty")
	}
	if err := os.MkdirAll(cfg.SaveDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create save directory %q: %w", cfg.SaveDir, err)
	}
	if cfg.Logger == nil {
		cfg.Logger = func(format string, args ...any) {
			fmt.Printf(format+"\n", args...)
		}
	}

	staticFS, err := web.StaticFS()
	if err != nil {
		return nil, fmt.Errorf("failed to load embedded web assets: %w", err)
	}

	s := &Server{
		cfg:          cfg,
		hub:          NewHub(),
		sessionToken: randomHex(24),
		mux:          http.NewServeMux(),
		staticFS:     staticFS,
		fileIndex:    make(map[string]FileEntry),
	}

	s.registerRoutes()
	return s, nil
}

// Handler returns the HTTP handler for net/http.Server.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// BaseURL returns the LAN base URL without query params.
func (s *Server) BaseURL() string {
	return fmt.Sprintf("http://%s:%d", s.cfg.Host, s.cfg.Port)
}

// ConnectURL returns the full QR-scannable URL, including ?pin=XXXX when PIN protection is enabled.
func (s *Server) ConnectURL() string {
	if s.cfg.PIN != "" {
		return fmt.Sprintf("%s/?pin=%s", s.BaseURL(), s.cfg.PIN)
	}
	return s.BaseURL() + "/"
}

// AddLocalFile registers a local PC file (e.g. via `airbeam send <file>`) for instant 1-tap phone download.
func (s *Server) AddLocalFile(filePath string) (FileEntry, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return FileEntry{}, fmt.Errorf("invalid path %q: %w", filePath, err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return FileEntry{}, fmt.Errorf("cannot access %q: %w", filePath, err)
	}
	if info.IsDir() {
		return FileEntry{}, fmt.Errorf("%q is a directory; pass individual files or a zip archive", filePath)
	}

	entry := buildFileEntry(filepath.Base(absPath), info.Size(), absPath, "pc", "PC")
	s.addFileEntry(entry)
	return entry, nil
}

// GeneratePIN generates a cryptographically random 4-digit PIN between 1000 and 9999.
func GeneratePIN() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%04d", 1000+(time.Now().UnixNano()%9000))
	}
	val := (int(b[0])<<8 | int(b[1])) % 9000
	return fmt.Sprintf("%04d", 1000+val)
}

func (s *Server) registerRoutes() {
	// Public / bootstrap endpoints
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("POST /api/auth", s.handleAuth)
	s.mux.HandleFunc("GET /api/qr.png", s.handleQRPNG)

	// Protected API endpoints
	s.mux.HandleFunc("GET /api/events", s.requireAuth(s.hub.ServeHTTP))
	s.mux.HandleFunc("GET /api/files", s.requireAuth(s.handleListFiles))
	s.mux.HandleFunc("POST /api/upload", s.requireAuth(s.handleUpload))
	s.mux.HandleFunc("GET /api/download/{id}", s.requireAuth(s.handleDownload))
	s.mux.HandleFunc("GET /api/clipboard", s.requireAuth(s.handleGetClipboard))
	s.mux.HandleFunc("POST /api/clipboard", s.requireAuth(s.handlePostClipboard))

	// Embedded Web UI (handles ?pin=XXXX auto-authentication on root load)
	s.mux.HandleFunc("GET /", s.handleWebUI)
}

func (s *Server) handleWebUI(w http.ResponseWriter, r *http.Request) {
	// Auto-authenticate when scanning the QR URL with ?pin=XXXX
	if pinParam := r.URL.Query().Get("pin"); pinParam != "" && s.cfg.PIN != "" {
		if s.verifyPIN(pinParam) {
			s.setAuthCookie(w)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}

	// Serve embedded asset if it exists; otherwise fall back to index.html.
	if f, err := s.staticFS.Open(path); err == nil {
		_ = f.Close()
		http.FileServer(http.FS(s.staticFS)).ServeHTTP(w, r)
		return
	}

	indexBytes, err := fs.ReadFile(s.staticFS, "index.html")
	if err != nil {
		http.Error(w, "UI unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexBytes)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	authed := s.isAuthenticated(r)
	resp := map[string]any{
		"ip":            s.cfg.Host,
		"port":          s.cfg.Port,
		"url":           s.BaseURL(),
		"pinRequired":   s.cfg.PIN != "",
		"authenticated": authed,
		"devices":       s.hub.ClientCount(),
	}
	if authed || isLoopbackRequest(r) {
		resp["connectUrl"] = s.ConnectURL()
		resp["saveDir"] = s.cfg.SaveDir
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PIN == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	var body struct {
		PIN string `json:"pin"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if !s.verifyPIN(strings.TrimSpace(body.PIN)) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "incorrect PIN"})
		return
	}

	s.setAuthCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleQRPNG(w http.ResponseWriter, r *http.Request) {
	// Allow QR PNG for authenticated devices or the local host PC dashboard.
	if !s.isAuthenticated(r) && !isLoopbackRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}

	pngData, err := qr.RenderPNG(s.ConnectURL(), 320)
	if err != nil {
		http.Error(w, "failed to generate QR code", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(pngData)
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	list := make([]FileEntry, len(s.files))
	copy(list, s.files)
	s.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"files": list,
	})
}

// handleUpload streams multipart file uploads directly to disk using io.CopyBuffer
// so multi-gigabyte 4K videos upload at full LAN speed with near-zero RAM usage.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected multipart/form-data upload"})
		return
	}

	device := detectDeviceLabel(r.UserAgent())
	source := "phone"
	if isLoopbackRequest(r) {
		source = "pc"
		device = "PC"
	}

	buf := make([]byte, copyBufferSize)
	var uploaded []FileEntry

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "upload interrupted: " + err.Error()})
			return
		}

		rawName := part.FileName()
		if rawName == "" {
			_ = part.Close()
			continue
		}

		safeName := sanitizeFilename(rawName)
		finalPath, finalName, err := reserveUniquePath(s.cfg.SaveDir, safeName)
		if err != nil {
			_ = part.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to reserve file path"})
			return
		}

		tmpPath := finalPath + ".airbeam.part"
		dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			_ = part.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create destination file"})
			return
		}

		start := time.Now()
		written, copyErr := io.CopyBuffer(dst, part, buf)
		closeErr := dst.Close()
		_ = part.Close()

		if copyErr != nil || closeErr != nil {
			_ = os.Remove(tmpPath)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed while streaming file to disk"})
			return
		}

		if err := os.Rename(tmpPath, finalPath); err != nil {
			_ = os.Remove(tmpPath)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to finalize uploaded file"})
			return
		}

		elapsed := time.Since(start)
		if elapsed < time.Millisecond {
			elapsed = time.Millisecond
		}
		mbPerSec := (float64(written) / (1024 * 1024)) / elapsed.Seconds()

		s.cfg.Logger("[RECEIVED] %s (%s) in %.1fs — %.1f MB/s [%s]",
			finalName, FormatBytes(written), elapsed.Seconds(), mbPerSec, device)

		entry := buildFileEntry(finalName, written, finalPath, source, device)
		s.addFileEntry(entry)
		uploaded = append(uploaded, entry)
	}

	if len(uploaded) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no files found in upload"})
		return
	}

	s.hub.Broadcast("files_updated", map[string]any{
		"action": "uploaded",
		"files":  uploaded,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"files": uploaded,
	})
}

// handleDownload streams shared or uploaded files with full HTTP Range support via http.ServeContent.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	s.mu.RLock()
	entry, ok := s.fileIndex[id]
	s.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(entry.DiskPath)
	if err != nil {
		http.Error(w, "file no longer available on disk", http.StatusGone)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "failed to stat file", http.StatusInternalServerError)
		return
	}

	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": entry.Name})
	if disposition == "" {
		disposition = fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(entry.Name, `"`, "_"))
	}
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")

	start := time.Now()
	http.ServeContent(w, r, entry.Name, info.ModTime(), f)

	// Log completed full-file downloads (skip noisy partial range probes).
	if r.Header.Get("Range") == "" {
		elapsed := time.Since(start)
		if elapsed < time.Millisecond {
			elapsed = time.Millisecond
		}
		mbPerSec := (float64(info.Size()) / (1024 * 1024)) / elapsed.Seconds()
		s.cfg.Logger("[SENT]     %s (%s) in %.1fs — %.1f MB/s -> %s",
			entry.Name, FormatBytes(info.Size()), elapsed.Seconds(), mbPerSec, detectDeviceLabel(r.UserAgent()))
	}
}

func (s *Server) handleGetClipboard(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	items := make([]ClipboardEntry, len(s.clipboard))
	copy(items, s.clipboard)
	s.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
	})
}

func (s *Server) handlePostClipboard(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text   string `json:"text"`
		Device string `json:"device"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxClipboardBytes)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid clipboard payload"})
		return
	}

	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "clipboard text cannot be empty"})
		return
	}

	device := strings.TrimSpace(body.Device)
	if device == "" {
		if isLoopbackRequest(r) {
			device = "PC"
		} else {
			device = detectDeviceLabel(r.UserAgent())
		}
	}

	entry := ClipboardEntry{
		ID:        randomHex(8),
		Text:      text,
		Device:    device,
		CreatedAt: time.Now(),
	}

	s.mu.Lock()
	s.clipboard = append([]ClipboardEntry{entry}, s.clipboard...)
	if len(s.clipboard) > maxClipboardEntries {
		s.clipboard = s.clipboard[:maxClipboardEntries]
	}
	s.mu.Unlock()

	preview := text
	if len(preview) > 72 {
		preview = preview[:72] + "..."
	}
	preview = strings.ReplaceAll(preview, "\n", " ")
	s.cfg.Logger("[CLIPBOARD] %s: %s", device, preview)

	s.hub.Broadcast("clipboard", entry)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"item": entry,
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "PIN authentication required"})
			return
		}
		next(w, r)
	}
}

func (s *Server) isAuthenticated(r *http.Request) bool {
	if s.cfg.PIN == "" {
		return true
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.sessionToken)) == 1 {
			return true
		}
	}
	if headerPIN := r.Header.Get("X-AirBeam-PIN"); headerPIN != "" && s.verifyPIN(headerPIN) {
		return true
	}
	if queryPIN := r.URL.Query().Get("pin"); queryPIN != "" && s.verifyPIN(queryPIN) {
		return true
	}
	return false
}

func (s *Server) verifyPIN(candidate string) bool {
	if s.cfg.PIN == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.cfg.PIN)) == 1
}

func (s *Server) setAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    s.sessionToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) addFileEntry(entry FileEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files = append([]FileEntry{entry}, s.files...)
	s.fileIndex[entry.ID] = entry
}

func buildFileEntry(name string, size int64, diskPath, source, device string) FileEntry {
	ext := strings.ToUpper(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "" {
		ext = "FILE"
	}
	mimeType := mime.TypeByExtension(filepath.Ext(name))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return FileEntry{
		ID:         randomHex(8),
		Name:       name,
		Size:       size,
		SizeHuman:  FormatBytes(size),
		Ext:        ext,
		MimeType:   mimeType,
		Source:     source,
		Device:     device,
		UploadedAt: time.Now(),
		DiskPath:   diskPath,
	}
}

// sanitizeFilename protects against directory traversal, Windows backslash paths, and control characters.
func sanitizeFilename(raw string) string {
	normalized := strings.ReplaceAll(raw, "\\", "/")
	base := filepath.Base(normalized)
	base = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '<' || r == '>' || r == ':' || r == '"' || r == '|' || r == '?' || r == '*' {
			return '_'
		}
		return r
	}, base)
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == ".." {
		return fmt.Sprintf("airbeam_%d.bin", time.Now().Unix())
	}
	return base
}

func reserveUniquePath(dir, name string) (string, string, error) {
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
		return candidate, name, nil
	}

	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 10000; i++ {
		altName := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		altPath := filepath.Join(dir, altName)
		if _, err := os.Stat(altPath); errors.Is(err, os.ErrNotExist) {
			return altPath, altName, nil
		}
	}
	return "", "", errors.New("could not allocate unique filename")
}

func detectDeviceLabel(ua string) string {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "iphone"):
		return "iPhone"
	case strings.Contains(l, "ipad"):
		return "iPad"
	case strings.Contains(l, "android"):
		return "Android"
	case strings.Contains(l, "macintosh") || strings.Contains(l, "mac os"):
		return "macOS"
	case strings.Contains(l, "windows"):
		return "Windows"
	case strings.Contains(l, "linux"):
		return "Linux"
	default:
		return "Device"
	}
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// FormatBytes renders a byte count as a crisp human-readable string (e.g. "142.8 MB").
func FormatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"KB", "MB", "GB", "TB"}
	if exp >= len(units) {
		exp = len(units) - 1
	}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), units[exp])
}

func randomHex(numBytes int) string {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
