package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestPathCandidates(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"app/calculator.go", []string{"app/calculator.go"}},
		{"/src/app/calculator.go", []string{"src/app/calculator.go", "app/calculator.go"}},
		{`C:\build\pkg\handler.go`, []string{"build/pkg/handler.go", "pkg/handler.go"}},
		{"./main.go", []string{"main.go"}},
		{"", nil},
	}
	for _, c := range cases {
		if got := PathCandidates(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("PathCandidates(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPathCandidatesIsBounded(t *testing.T) {
	deep := "/" + strings.Repeat("d/", 20) + "f.go"
	if n := len(PathCandidates(deep)); n != maxPathCandidates {
		t.Fatalf("expected %d candidates, got %d", maxPathCandidates, n)
	}
}

// fakeRepo serves the raw-file endpoint for a fixed set of repo paths.
func fakeRepo(t *testing.T, files map[string]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p := r.URL.EscapedPath()
		const prefix = "/api/v4/projects/42/repository/files/"
		if !strings.HasPrefix(p, prefix) || !strings.HasSuffix(p, "/raw") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		filePath, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/raw"))
		content, ok := files[filePath]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 File Not Found"}`))
			return
		}
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok", "42", nil)
}

func TestResolveFileStripsStackTracePrefix(t *testing.T) {
	c := fakeRepo(t, map[string]string{"app/calculator.go": "package app"})

	path, content, err := c.ResolveFile(context.Background(), "/src/app/calculator.go", "main")
	if err != nil {
		t.Fatalf("ResolveFile: %v", err)
	}
	if path != "app/calculator.go" || content != "package app" {
		t.Fatalf("got %q / %q", path, content)
	}
}

func TestResolveFileNotFound(t *testing.T) {
	c := fakeRepo(t, map[string]string{"app/other.go": "x"})

	_, _, err := c.ResolveFile(context.Background(), "/src/app/calculator.go", "main")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}
}
