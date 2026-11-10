// Local web frontend for Envelopely. Run from the project root: go run ./webui/server.go
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

//go:embed index.html settings.html help.html envelopely-logo.svg envelopely-logo-dark-icon.png envelopely-logo-dark-wide.png envelopely-logo-dark-transparent.png
var assets embed.FS

var safeName = regexp.MustCompile(`[^A-Za-z0-9._ -]+`)

// kinds maps an upload kind to its directory and allowed extensions.
var kinds = map[string]struct {
	dir  string
	exts []string
}{
	"template":   {"templates", []string{".txt"}},
	"attachment": {"attachments", []string{".pdf", ".doc", ".docx", ".txt"}},
	"contacts":   {"contacts", []string{".csv"}},
}

func cleanName(name string, exts []string) (string, bool) {
	name = safeName.ReplaceAllString(filepath.Base(name), "_")
	name = strings.TrimLeft(name, ".")
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range exts {
		if ext == e {
			return name, true
		}
	}
	return "", false
}

func listDir(dir string, exts []string) []string {
	out := []string{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		for _, x := range exts {
			if ext == x {
				out = append(out, e.Name())
			}
		}
	}
	return out
}

// resolve returns a path inside the kind's directory for an existing file name.
func resolve(kind, name string) (string, bool) {
	k, ok := kinds[kind]
	if !ok {
		return "", false
	}
	clean, ok := cleanName(name, k.exts)
	if !ok || clean != name {
		return "", false
	}
	p := filepath.Join(k.dir, clean)
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		return "", false
	}
	return p, true
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}

// sameOrigin blocks cross-site requests against the local server.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					fail(w, http.StatusForbidden, "origin not allowed")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func handleState(w http.ResponseWriter, r *http.Request) {
	state := map[string]any{}
	for kind, k := range kinds {
		state[kind] = listDir(k.dir, k.exts)
	}
	name := strings.TrimSpace(configValue("MAIL_FROM_NAME"))
	if strings.EqualFold(name, "Your Name") {
		name = ""
	}
	state["profile"] = map[string]any{
		"name": name,
		"smtpConfigured": configValue("MAIL_SMTP_HOST") != "" &&
			configValue("MAIL_FROM") != "",
	}
	jsonOut(w, state)
}

// configValue prefers a non-empty process value and falls back to the local
// .env file. Callers must request only values that are safe to expose or use
// the result only as a boolean; credential values are never serialized.
func configValue(wanted string) string {
	if value := strings.TrimSpace(os.Getenv(wanted)); value != "" {
		return value
	}
	b, err := os.ReadFile(localEnvPath())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == wanted {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

func localEnvPath() string {
	if path := strings.TrimSpace(os.Getenv("MAIL_ENV_FILE")); path != "" {
		return path
	}
	if _, err := os.Stat(".env"); err == nil {
		return ".env"
	}
	if _, err := os.Stat(".env.local"); err == nil {
		return ".env.local"
	}
	return ".env"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

type settingsRequest struct {
	SMTPHost string `json:"smtpHost"`
	SMTPPort string `json:"smtpPort"`
	SMTPUser string `json:"smtpUser"`
	SMTPPass string `json:"smtpPass"`
	From     string `json:"from"`
	FromName string `json:"fromName"`
	ReplyTo  string `json:"replyTo"`
	SMTPTLS  string `json:"smtpTLS"`
}

func handleSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		jsonOut(w, map[string]any{
			"smtpHost":    configValue("MAIL_SMTP_HOST"),
			"smtpPort":    firstNonEmpty(configValue("MAIL_SMTP_PORT"), "587"),
			"smtpUser":    configValue("MAIL_SMTP_USER"),
			"passwordSet": configValue("MAIL_SMTP_PASS") != "",
			"from":        configValue("MAIL_FROM"),
			"fromName":    configValue("MAIL_FROM_NAME"),
			"replyTo":     configValue("MAIL_REPLY_TO"),
			"smtpTLS":     configValue("MAIL_SMTP_TLS"),
		})
		return
	}
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req settingsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "invalid settings")
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(req.SMTPPort))
	if err != nil || port < 1 || port > 65535 {
		fail(w, http.StatusBadRequest, "SMTP port must be between 1 and 65535")
		return
	}
	values := map[string]string{
		"MAIL_SMTP_HOST": strings.TrimSpace(req.SMTPHost),
		"MAIL_SMTP_PORT": strconv.Itoa(port),
		"MAIL_SMTP_USER": strings.TrimSpace(req.SMTPUser),
		"MAIL_FROM":      strings.TrimSpace(req.From),
		"MAIL_FROM_NAME": strings.TrimSpace(req.FromName),
		"MAIL_REPLY_TO":  strings.TrimSpace(req.ReplyTo),
		"MAIL_SMTP_TLS":  strings.TrimSpace(req.SMTPTLS),
	}
	if req.SMTPPass != "" {
		values["MAIL_SMTP_PASS"] = req.SMTPPass
	}
	for key, value := range values {
		if strings.ContainsAny(value, "\r\n") {
			fail(w, http.StatusBadRequest, "settings must not contain line breaks")
			return
		}
		_ = os.Setenv(key, value)
	}
	if err := updateEnvFile(localEnvPath(), values); err != nil {
		fail(w, http.StatusInternalServerError, "could not save local settings")
		return
	}
	jsonOut(w, map[string]bool{"saved": true})
}

func updateEnvFile(path string, values map[string]string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	written := map[string]bool{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		key, _, ok := strings.Cut(trimmed, "=")
		key = strings.TrimSpace(key)
		if !ok || strings.HasPrefix(trimmed, "#") || written[key] {
			continue
		}
		if value, exists := values[key]; exists {
			lines[i] = key + "=" + value
			written[key] = true
		}
	}
	for key, value := range values {
		if !written[key] {
			lines = append(lines, key+"="+value)
		}
	}
	content := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// loadLocalEnv makes the same local .env values available to the Web UI
// without exposing credential values through the state endpoint.
func loadLocalEnv(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				_ = os.Setenv(key, value)
			}
		}
	}
	return nil
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20)
	k, ok := kinds[r.URL.Query().Get("kind")]
	if !ok {
		fail(w, 400, "unknown kind")
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	defer f.Close()
	name, ok := cleanName(hdr.Filename, k.exts)
	if !ok {
		fail(w, 400, "unsupported file type, allowed: "+strings.Join(k.exts, ", "))
		return
	}
	if err := os.MkdirAll(k.dir, 0o755); err != nil {
		fail(w, 500, err.Error())
		return
	}
	out, err := os.Create(filepath.Join(k.dir, name))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, f); err != nil {
		fail(w, 500, err.Error())
		return
	}
	jsonOut(w, map[string]string{"name": name})
}

func handleTemplate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p, ok := resolve("template", r.URL.Query().Get("name"))
		if !ok {
			fail(w, 404, "template not found")
			return
		}
		b, _ := os.ReadFile(p)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(b)
	case http.MethodPost:
		var req struct{ Name, Content string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			fail(w, 400, err.Error())
			return
		}
		name := req.Name
		if !strings.HasSuffix(strings.ToLower(name), ".txt") {
			name += ".txt"
		}
		name, ok := cleanName(name, []string{".txt"})
		if !ok || strings.TrimSpace(req.Content) == "" {
			fail(w, 400, "name and content are required")
			return
		}
		os.MkdirAll("templates", 0o755)
		if err := os.WriteFile(filepath.Join("templates", name), []byte(req.Content), 0o644); err != nil {
			fail(w, 500, err.Error())
			return
		}
		jsonOut(w, map[string]string{"name": name})
	default:
		fail(w, 405, "method not allowed")
	}
}

type runRequest struct {
	Contacts     string `json:"contacts"`
	Template     string `json:"template"`
	Attachment   string `json:"attachment"`
	Mode         string `json:"mode"` // check, preview, send
	Limit        int    `json:"limit"`
	Language     string `json:"language"`
	Audience     string `json:"audience"`
	Delay        int    `json:"delay"`
	Status       string `json:"status"`
	ConfirmNum   int    `json:"confirm"`
	AllowGeneric bool   `json:"allowGeneric"`
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	contacts, ok1 := resolve("contacts", req.Contacts)
	tpl, ok2 := resolve("template", req.Template)
	if !ok1 || !ok2 {
		fail(w, 400, "select a valid contacts list and template")
		return
	}
	if req.Language != "it" && req.Language != "en" {
		req.Language = "en"
	}
	if req.Audience != "technical" && req.Audience != "nontechnical" {
		req.Audience = "nontechnical"
	}
	if req.Limit < 0 || req.Delay < 0 {
		fail(w, 400, "invalid limit or delay")
		return
	}
	args := []string{
		"--contacts", contacts,
		"--template", tpl, "--template-it=", "--template-en=",
		"--language", req.Language, "--audience", req.Audience,
		"--limit", strconv.Itoa(req.Limit),
		"--delay", strconv.Itoa(req.Delay) + "s",
	}
	if s := strings.TrimSpace(req.Status); s != "" {
		args = append(args, "--eligible-status", s)
	}
	if req.AllowGeneric {
		args = append(args, "--allow-generic")
	}
	attachment, ok := resolve("attachment", req.Attachment)
	if !ok {
		fail(w, 400, "select a valid attachment")
		return
	}
	args = append(args, "--attachment", attachment)
	switch req.Mode {
	case "check":
		args = append(args, "--check")
	case "preview":
		args = append(args, "--preview-dir", "previews/web", "--output", "previews/web/contacts_preview.csv")
		os.MkdirAll("previews/web", 0o755)
	case "send":
		if req.ConfirmNum <= 0 {
			fail(w, 400, "missing confirmation")
			return
		}
		// Re-run preflight server-side and require the exact eligible count before bypassing
		// the terminal confirmation. This keeps the browser flow review-first.
		checkArgs := append(append([]string{}, args...), "--check")
		checkOut, err := exec.Command("./envelopely", checkArgs...).CombinedOutput()
		if err != nil {
			fail(w, 400, "preflight failed:\n"+string(checkOut))
			return
		}
		match := regexp.MustCompile(`WILL SEND THIS RUN:\s+(\d+)`).FindStringSubmatch(string(checkOut))
		if len(match) != 2 {
			fail(w, 500, "could not read the preflight send count")
			return
		}
		expected, _ := strconv.Atoi(match[1])
		if req.ConfirmNum != expected {
			fail(w, 400, fmt.Sprintf("confirmation mismatch: preflight reports %d eligible message(s)", expected))
			return
		}
		args = append(args, "--send", "--yes")
	default:
		fail(w, 400, "unknown mode")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	cmd := exec.CommandContext(r.Context(), "./envelopely", args...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	done := make(chan error, 1)
	go func() { err := cmd.Wait(); pw.Close(); done <- err }()
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if err := <-done; err != nil {
		fmt.Fprintf(w, "\n[exit: %v]\n", err)
	}
}

func main() {
	if err := loadLocalEnv(localEnvPath()); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: could not read .env: %v", err)
	}
	if _, err := os.Stat("main.go"); err != nil {
		log.Fatal("run from the project root: go run ./webui/server.go")
	}
	if out, err := exec.Command("go", "build", "-o", "envelopely", ".").CombinedOutput(); err != nil {
		log.Fatalf("build failed: %v\n%s", err, out)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServerFS(assets))
	mux.HandleFunc("/api/state", handleState)
	mux.HandleFunc("/api/upload", handleUpload)
	mux.HandleFunc("/api/template", handleTemplate)
	mux.HandleFunc("/api/settings", handleSettings)
	mux.HandleFunc("/api/run", handleRun)
	addr := "127.0.0.1:8080"
	if a := os.Getenv("ENVELOPELY_WEB_ADDR"); a != "" {
		addr = a
	}
	log.Printf("open http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, sameOrigin(mux)))
}
