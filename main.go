package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const defaultDataDir = "/data"

var safePart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type server struct {
	dataDir string
	token   string
	mu      sync.Mutex
}

func main() {
	dataDir := os.Getenv("REPOSITORY_DATA_DIR")
	if dataDir == "" {
		dataDir = defaultDataDir
	}

	s := &server{dataDir: dataDir, token: os.Getenv("PUBLISH_TOKEN")}
	if s.token == "" {
		log.Fatal("PUBLISH_TOKEN must be set")
	}
	if err := s.initialize(); err != nil {
		log.Fatalf("repository initialization failed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /api/v1/packages/debian", s.uploadDebian)
	mux.HandleFunc("POST /api/v1/packages/rpm", s.uploadRPM)
	mux.HandleFunc("POST /api/v1/packages/arch", s.uploadArch)
	mux.Handle("/", s.static())

	addr := ":8080"
	log.Printf("linux-repo listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, securityHeaders(mux)))
}

func (s *server) initialize() error {
	for _, dir := range []string{
		s.dataDir,
		filepath.Join(s.dataDir, "keys"),
		filepath.Join(s.dataDir, "incoming"),
		filepath.Join(s.dataDir, "debian", "conf"),
		filepath.Join(s.dataDir, "debian", "dists"),
		filepath.Join(s.dataDir, "debian", "pool"),
		filepath.Join(s.dataDir, "rpm", "fedora"),
		filepath.Join(s.dataDir, "arch", "x86_64"),
		filepath.Join(s.dataDir, "arch", "aarch64"),
		filepath.Join(s.dataDir, ".gnupg"),
	} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	if err := os.Chmod(filepath.Join(s.dataDir, ".gnupg"), 0700); err != nil {
		return err
	}

	gnupg := filepath.Join(s.dataDir, ".gnupg")
	if err := run(gnupg, "gpg", "--batch", "--list-secret-keys"); err != nil {
		return err
	}
	keys, err := commandOutput(gnupg, "gpg", "--batch", "--with-colons", "--list-secret-keys")
	if err != nil {
		return err
	}
	fingerprint := firstFingerprint(keys)
	if fingerprint == "" {
		identity := os.Getenv("REPOSITORY_GPG_IDENTITY")
		if identity == "" {
			identity = "KingIronMan Linux Repository <packages@kingironman.dev>"
		}
		if err := run(gnupg, "gpg", "--batch", "--passphrase", "", "--quick-generate-key", identity, "rsa4096", "sign", "0"); err != nil {
			return err
		}
		keys, err = commandOutput(gnupg, "gpg", "--batch", "--with-colons", "--list-secret-keys")
		if err != nil {
			return err
		}
		fingerprint = firstFingerprint(keys)
	}
	if fingerprint == "" {
		return errors.New("could not determine repository signing key fingerprint")
	}

	distributions := fmt.Sprintf("Origin: KingIronMan\nLabel: KingIronMan Linux Repository\nCodename: stable\nArchitectures: amd64 arm64\nComponents: main\nDescription: KingIronMan Linux packages\nSignWith: %s\n", fingerprint)
	if err := os.WriteFile(filepath.Join(s.dataDir, "debian", "conf", "distributions"), []byte(distributions), 0644); err != nil {
		return err
	}
	if err := exportKey(gnupg, fingerprint, filepath.Join(s.dataDir, "keys", "linux-repo.asc"), true); err != nil {
		return err
	}
	return exportKey(gnupg, fingerprint, filepath.Join(s.dataDir, "keys", "linux-repo.gpg"), false)
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) uploadDebian(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	if r.URL.Query().Get("suite") != "" && r.URL.Query().Get("suite") != "stable" {
		http.Error(w, "only suite=stable is configured", http.StatusBadRequest)
		return
	}
	path, name, err := s.receive(r, "package", ".deb")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer os.Remove(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := run(filepath.Join(s.dataDir, ".gnupg"), "reprepro", "-b", filepath.Join(s.dataDir, "debian"), "includedeb", "stable", path); err != nil {
		http.Error(w, "could not publish Debian package", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"published": name, "repository": "debian"})
}

func (s *server) uploadRPM(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	release, arch := r.URL.Query().Get("release"), r.URL.Query().Get("arch")
	if !valid(release) || !valid(arch) {
		http.Error(w, "release and arch are required", http.StatusBadRequest)
		return
	}
	path, name, err := s.receive(r, "package", ".rpm")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer os.Remove(path)
	repoDir := filepath.Join(s.dataDir, "rpm", "fedora", release, arch)
	packagesDir := filepath.Join(repoDir, "Packages")
	if err := os.MkdirAll(packagesDir, 0755); err != nil {
		http.Error(w, "could not create RPM repository", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(path, filepath.Join(packagesDir, name)); err != nil {
		http.Error(w, "could not store RPM package", http.StatusInternalServerError)
		return
	}
	if err := run("", "createrepo_c", "--update", repoDir); err != nil {
		http.Error(w, "could not update RPM metadata", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"published": name, "repository": "rpm"})
}

func (s *server) uploadArch(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	arch := r.URL.Query().Get("arch")
	if !valid(arch) {
		http.Error(w, "arch is required", http.StatusBadRequest)
		return
	}
	path, name, err := s.receive(r, "package", ".pkg.tar.zst")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer os.Remove(path)
	repoDir := filepath.Join(s.dataDir, "arch", arch)
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		http.Error(w, "could not create Arch repository", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target := filepath.Join(repoDir, name)
	if err := os.Rename(path, target); err != nil {
		http.Error(w, "could not store Arch package", http.StatusInternalServerError)
		return
	}
	if err := run("", "repo-add", "--include-sigs", filepath.Join(repoDir, "linux-repo.db.tar.zst"), target); err != nil {
		http.Error(w, "could not update pacman metadata", http.StatusInternalServerError)
		return
	}
	for _, file := range []string{"linux-repo.db", "linux-repo.files"} {
		_ = os.Remove(filepath.Join(repoDir, file))
	}
	if err := os.Symlink("linux-repo.db.tar.zst", filepath.Join(repoDir, "linux-repo.db")); err != nil {
		return
	}
	_ = os.Symlink("linux-repo.files.tar.zst", filepath.Join(repoDir, "linux-repo.files"))
	writeJSON(w, http.StatusCreated, map[string]string{"published": name, "repository": "arch"})
}

func (s *server) receive(r *http.Request, field, suffix string) (string, string, error) {
	if err := r.ParseMultipartForm(2 << 30); err != nil {
		return "", "", errors.New("invalid multipart upload")
	}
	file, header, err := r.FormFile(field)
	if err != nil {
		return "", "", errors.New("package file is required")
	}
	defer file.Close()
	name := filepath.Base(header.Filename)
	if !strings.HasSuffix(name, suffix) || !safePart.MatchString(name) {
		return "", "", fmt.Errorf("package must be a %s file with a safe filename", suffix)
	}
	path := filepath.Join(s.dataDir, "incoming", name)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", "", err
	}
	_, copyErr := io.Copy(out, file)
	closeErr := out.Close()
	if copyErr != nil {
		return "", "", copyErr
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	return path, name, nil
}

func (s *server) authorized(w http.ResponseWriter, r *http.Request) bool {
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(provided) != len(s.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *server) static() http.Handler {
	return http.FileServer(http.Dir(s.dataDir))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.Contains(r.URL.Path, "Release") || strings.Contains(r.URL.Path, "repodata") || strings.HasSuffix(r.URL.Path, ".db") {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		} else if strings.HasSuffix(r.URL.Path, ".deb") || strings.HasSuffix(r.URL.Path, ".rpm") || strings.HasSuffix(r.URL.Path, ".zst") {
			w.Header().Set("Cache-Control", "public, immutable, max-age=2592000")
		}
		next.ServeHTTP(w, r)
	})
}

func valid(value string) bool { return safePart.MatchString(value) }

func run(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if dir != "" {
		cmd.Env = append(os.Environ(), "GNUPGHOME="+dir)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("%s failed: %s", name, strings.TrimSpace(string(output)))
		return err
	}
	return nil
}

func commandOutput(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GNUPGHOME="+dir)
	output, err := cmd.Output()
	return string(output), err
}

func firstFingerprint(keys string) string {
	for _, line := range strings.Split(keys, "\n") {
		parts := strings.Split(line, ":")
		if len(parts) > 9 && parts[0] == "fpr" {
			return parts[9]
		}
	}
	return ""
}

func exportKey(home, fingerprint, target string, armor bool) error {
	args := []string{"--batch"}
	if armor {
		args = append(args, "--armor")
	}
	args = append(args, "--export", fingerprint)
	cmd := exec.Command("gpg", args...)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
	output, err := cmd.Output()
	if err != nil {
		return err
	}
	return os.WriteFile(target, output, 0644)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
