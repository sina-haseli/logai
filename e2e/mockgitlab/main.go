// Command mockgitlab is a tiny in-memory stand-in for a self-hosted GitLab
// instance. It implements only the REST v4 endpoints that logai calls:
//
//	GET  /api/v4/projects/:id/repository/files/:path/raw?ref=
//	POST /api/v4/projects/:id/repository/branches?branch=&ref=
//	PUT  /api/v4/projects/:id/repository/files/:path
//	POST /api/v4/projects/:id/merge_requests
//
// Plus a couple of /_debug/* endpoints the E2E harness uses to assert results.
//
// It is intentionally dependency-free (stdlib only) and stores everything in
// memory. NOT for production use.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type server struct {
	mu sync.Mutex
	// files[branch][path] = content
	files map[string]map[string]string
	mrs   []mergeRequest
	mrSeq int
}

type mergeRequest struct {
	IID          int       `json:"iid"`
	WebURL       string    `json:"web_url"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	SourceBranch string    `json:"source_branch"`
	TargetBranch string    `json:"target_branch"`
	CreatedAt    time.Time `json:"created_at"`
}

const (
	defaultBranch = "main"
	seedPath      = "app/calculator.go"
)

// The seeded "buggy" source file: Divide panics on b == 0.
const seedContent = `package app

// Divide returns the integer quotient of a divided by b.
func Divide(a, b int) int {
	return a / b
}
`

func newServer() *server {
	s := &server{files: map[string]map[string]string{}}
	s.files[defaultBranch] = map[string]string{seedPath: seedContent}
	return s
}

var (
	reRaw      = regexp.MustCompile(`^/api/v4/projects/([^/]+)/repository/files/(.+)/raw$`)
	reFiles    = regexp.MustCompile(`^/api/v4/projects/([^/]+)/repository/files/(.+)$`)
	reBranches = regexp.MustCompile(`^/api/v4/projects/([^/]+)/repository/branches$`)
	reMRs      = regexp.MustCompile(`^/api/v4/projects/([^/]+)/merge_requests$`)
)

func main() {
	addr := ":8080"
	if v := os.Getenv("PORT"); v != "" {
		addr = ":" + v
	}
	s := newServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.route)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("/_debug/file", s.debugFile)
	mux.HandleFunc("/_debug/mrs", s.debugMRs)

	log.Printf("mockgitlab listening on %s (seeded %s on %s)", addr, seedPath, defaultBranch)
	if err := http.ListenAndServe(addr, logging(mux)); err != nil {
		log.Fatal(err)
	}
}

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	// Use EscapedPath so url-encoded %2F in file paths survives.
	path := r.URL.EscapedPath()

	switch {
	case r.Method == http.MethodGet && reRaw.MatchString(path):
		s.getRaw(w, r, reRaw.FindStringSubmatch(path))
	case r.Method == http.MethodPut && reFiles.MatchString(path):
		s.putFile(w, r, reFiles.FindStringSubmatch(path))
	case r.Method == http.MethodPost && reBranches.MatchString(path):
		s.createBranch(w, r)
	case r.Method == http.MethodPost && reMRs.MatchString(path):
		s.createMR(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Not Found: " + r.Method + " " + path})
	}
}

func (s *server) getRaw(w http.ResponseWriter, r *http.Request, m []string) {
	filePath, _ := url.PathUnescape(m[2])
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = defaultBranch
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	content, ok := s.lookup(ref, filePath)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 File Not Found"})
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

// lookup falls back to the default branch when a branch has no override, and is
// tolerant of path-shape differences (leading slashes, src/ prefixes) that an
// LLM might emit when localizing from a stack trace.
func (s *server) lookup(ref, path string) (string, bool) {
	branches := []map[string]string{}
	if b, ok := s.files[ref]; ok {
		branches = append(branches, b)
	}
	if ref != defaultBranch {
		branches = append(branches, s.files[defaultBranch])
	}

	for _, b := range branches {
		if c, ok := b[path]; ok {
			return c, true
		}
	}
	// Tolerant suffix/basename match.
	want := normPath(path)
	for _, b := range branches {
		for k, c := range b {
			nk := normPath(k)
			if nk == want || strings.HasSuffix(nk, "/"+want) || strings.HasSuffix(want, "/"+nk) || base(nk) == base(want) {
				return c, true
			}
		}
	}
	return "", false
}

func normPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimPrefix(p, "src/")
	return p
}

func base(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (s *server) putFile(w http.ResponseWriter, r *http.Request, m []string) {
	filePath, _ := url.PathUnescape(m[2])
	var body struct {
		Branch        string `json:"branch"`
		Content       string `json:"content"`
		CommitMessage string `json:"commit_message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad json"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files[body.Branch] == nil {
		s.files[body.Branch] = map[string]string{}
	}
	s.files[body.Branch][filePath] = body.Content

	writeJSON(w, http.StatusOK, map[string]string{
		"file_path": filePath,
		"branch":    body.Branch,
	})
}

func (s *server) createBranch(w http.ResponseWriter, r *http.Request) {
	branch := r.URL.Query().Get("branch")
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = defaultBranch
	}
	if branch == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "branch required"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Copy the ref's files (or default) into the new branch.
	src := s.files[ref]
	if src == nil {
		src = s.files[defaultBranch]
	}
	cp := map[string]string{}
	for k, v := range src {
		cp[k] = v
	}
	s.files[branch] = cp

	writeJSON(w, http.StatusCreated, map[string]any{
		"name":   branch,
		"commit": map[string]string{"id": "deadbeef"},
	})
}

func (s *server) createMR(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		Title        string `json:"title"`
		Description  string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad json"})
		return
	}

	s.mu.Lock()
	s.mrSeq++
	mr := mergeRequest{
		IID:          s.mrSeq,
		Title:        body.Title,
		Description:  body.Description,
		SourceBranch: body.SourceBranch,
		TargetBranch: body.TargetBranch,
		CreatedAt:    time.Now().UTC(),
	}
	mr.WebURL = fmt.Sprintf("http://mock-gitlab:8080/example/project/-/merge_requests/%d", mr.IID)
	s.mrs = append(s.mrs, mr)
	s.mu.Unlock()

	log.Printf("MR !%d opened: %q (%s -> %s)", mr.IID, mr.Title, mr.SourceBranch, mr.TargetBranch)
	writeJSON(w, http.StatusCreated, mr)
}

// --- debug endpoints used by the E2E harness ---

func (s *server) debugFile(w http.ResponseWriter, r *http.Request) {
	branch := r.URL.Query().Get("branch")
	path := r.URL.Query().Get("path")
	if branch == "" {
		branch = defaultBranch
	}
	if path == "" {
		path = seedPath
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.lookup(branch, path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"branch": branch, "path": path, "content": content})
}

func (s *server) debugMRs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.mrs)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/health") {
			log.Printf("%s %s", r.Method, r.URL.EscapedPath())
		}
		next.ServeHTTP(w, r)
	})
}
