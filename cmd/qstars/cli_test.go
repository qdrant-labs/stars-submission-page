package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a bytes.Buffer that is safe to read while the command writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// fakeServer follows the same contract as the real endpoints in cli_auth.go.
type fakeServer struct {
	*httptest.Server
	mu        sync.Mutex
	challenge string
	revoked   bool
	submitted []map[string]string
	deleted   []string
}

const fakeToken = "tok-123"

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/cli", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.challenge = q.Get("challenge")
		f.mu.Unlock()
		// the "browser login" succeeds immediately, then redirects to the CLI
		http.Redirect(w, r, "http://127.0.0.1:"+q.Get("port")+"/callback?code=one-time&state="+q.Get("state"), http.StatusFound)
	})
	mux.HandleFunc("POST /auth/cli/token", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Code, Verifier string }
		json.NewDecoder(r.Body).Decode(&req)
		sum := sha256.Sum256([]byte(req.Verifier))
		f.mu.Lock()
		ok := req.Code == "one-time" && base64.RawURLEncoding.EncodeToString(sum[:]) == f.challenge
		f.mu.Unlock()
		if !ok {
			http.Error(w, "invalid or expired code", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": fakeToken})
	})
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			revoked := f.revoked
			f.mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer "+fakeToken || revoked {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /api/me", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"preferred_username": "ada", "email": "ada@example.com"})
	}))
	mux.HandleFunc("GET /api/submissions", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"submissions": []map[string]any{
			{"id": "cont-old", "type": "content", "url": "https://a.example", "description": "older\npost", "timestamp": "2026-10-01T10:00:00Z"},
			{"id": "ev-new", "type": "event", "description": "a meetup", "timestamp": "2026-10-02T10:00:00Z"},
		}})
	}))
	mux.HandleFunc("GET /api/admin/submissions", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"submissions": []map[string]any{
			{"id": "x1", "type": "code", "owner_username": "bob", "description": "d", "timestamp": "2026-10-03T10:00:00Z"},
		}})
	}))
	mux.HandleFunc("GET /api/quota", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]int{"content": 4, "code": 10})
	}))
	mux.HandleFunc("POST /api/submissions", authed(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
			mr, _ := r.MultipartReader()
			var names []string
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(part)
				if part.FormName() == "data" {
					json.Unmarshal(data, &req)
				} else {
					names = append(names, part.FileName()+":"+http.DetectContentType(data))
				}
			}
			req["files"] = strings.Join(names, ",")
		} else {
			json.NewDecoder(r.Body).Decode(&req)
		}
		f.mu.Lock()
		f.submitted = append(f.submitted, req)
		n := len(f.submitted)
		f.mu.Unlock()
		if req["description"] == "over the limit" {
			http.Error(w, "you already reached the maximum number of content pieces you can submit for review during this month: 5", http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("cont-%d", n)})
	}))
	mux.HandleFunc("GET /api/submissions/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "cont-fresh":
			json.NewEncoder(w).Encode(map[string]any{"id": "cont-fresh", "type": "content", "description": "fresh", "timestamp": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)})
		case "cont-stale":
			json.NewEncoder(w).Encode(map[string]any{"id": "cont-stale", "type": "content", "description": "stale", "timestamp": time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)})
		default:
			http.Error(w, "submission does not exist: "+r.PathValue("id"), http.StatusNotFound)
		}
	}))
	mux.HandleFunc("DELETE /api/submissions/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /auth/logout", authed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.revoked = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// run executes the CLI with the given args and returns its output.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	out := &syncBuf{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

var urlRe = regexp.MustCompile(`http://127\.0\.0\.1:\d+/auth/cli\?\S+|http://[^\s]+/auth/cli\?\S+`)

func TestLoginListLogout(t *testing.T) {
	t.Setenv("QSTARS_CONFIG", t.TempDir()+"/qstars/config.json")
	f := newFakeServer(t)

	// not logged in yet
	if _, err := run(t, "list"); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("list before login: %v", err)
	}

	// login: run it, then play the browser by opening the printed URL
	root := newRootCmd()
	out := &syncBuf{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"login", "--server", f.URL, "--no-browser"})
	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	var loginURL string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && loginURL == "" {
		loginURL = urlRe.FindString(out.String())
		time.Sleep(10 * time.Millisecond)
	}
	if loginURL == "" {
		t.Fatalf("no login URL printed:\n%s", out.String())
	}
	res, err := http.Get(loginURL) // follows the redirect to the CLI's loopback server
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d", res.StatusCode)
	}
	if err := <-done; err != nil {
		t.Fatalf("login: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Logged in as ada <ada@example.com>") {
		t.Fatalf("output: %s", out.String())
	}
	p, _ := configPath()
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v %v", st, err)
	}

	// list: newest first, long text collapsed to one line
	got, err := run(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(got, "ev-new") > strings.Index(got, "cont-old") || !strings.Contains(got, "older post") || strings.Contains(got, "OWNER") {
		t.Fatalf("list output:\n%s", got)
	}
	got, err = run(t, "list", "--all")
	if err != nil || !strings.Contains(got, "OWNER") || !strings.Contains(got, "bob") {
		t.Fatalf("list --all: %v\n%s", err, got)
	}

	// logout revokes on the server and removes the local file
	if got, err := run(t, "logout"); err != nil || !strings.Contains(got, "Logged out") {
		t.Fatalf("logout: %v %s", err, got)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("config still there: %v", err)
	}
	f.mu.Lock()
	revoked := f.revoked
	f.mu.Unlock()
	if !revoked {
		t.Fatal("server session was not revoked")
	}
	if got, err := run(t, "logout"); err != nil || !strings.Contains(got, "not logged in") {
		t.Fatalf("second logout: %v %s", err, got)
	}
}

func TestListWithExpiredSession(t *testing.T) {
	t.Setenv("QSTARS_CONFIG", t.TempDir()+"/config.json")
	f := newFakeServer(t)
	if err := saveConfig(&config{Server: f.URL, Token: "stale"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "list"); err == nil || !strings.Contains(err.Error(), "qstars login") {
		t.Fatalf("expired session: %v", err)
	}
}

func TestLoginRejectsWrongState(t *testing.T) {
	// a callback with a state the CLI did not generate must be refused
	t.Setenv("QSTARS_CONFIG", t.TempDir()+"/config.json")
	f := newFakeServer(t)
	root := newRootCmd()
	out := &syncBuf{}
	root.SetOut(out)
	root.SetArgs([]string{"login", "--server", f.URL, "--no-browser"})
	go root.Execute()
	var loginURL string
	for i := 0; i < 500 && loginURL == ""; i++ {
		loginURL = urlRe.FindString(out.String())
		time.Sleep(10 * time.Millisecond)
	}
	port := regexp.MustCompile(`port=(\d+)`).FindStringSubmatch(loginURL)
	if port == nil {
		t.Fatalf("no port in %q", loginURL)
	}
	res, err := http.Get("http://127.0.0.1:" + port[1] + "/callback?code=one-time&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state: %d", res.StatusCode)
	}
}

func loggedIn(t *testing.T) *fakeServer {
	t.Helper()
	t.Setenv("QSTARS_CONFIG", t.TempDir()+"/config.json")
	f := newFakeServer(t)
	if err := saveConfig(&config{Server: f.URL, Token: fakeToken}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestQuota(t *testing.T) {
	loggedIn(t)
	got, err := run(t, "quota")
	if err != nil || !strings.Contains(got, "content: 4 left") || !strings.Contains(got, "code:    10 left") {
		t.Fatalf("%v\n%s", err, got)
	}
}

func TestSubmit(t *testing.T) {
	f := loggedIn(t)
	got, err := run(t, "submit", "-t", "content", "-u", " https://a.example ", "-d", "  my post  ")
	if err != nil || !strings.Contains(got, "Submitted cont-1") {
		t.Fatalf("%v\n%s", err, got)
	}
	want := map[string]string{"type": "content", "url": "https://a.example", "description": "my post"}
	if fmt.Sprint(f.submitted[0]) != fmt.Sprint(want) {
		t.Fatalf("sent %v, want %v", f.submitted[0], want)
	}

	// no link: the url field is left out; description from stdin
	root := newRootCmd()
	out := &syncBuf{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetIn(strings.NewReader("a meetup\nin Berlin\n"))
	root.SetArgs([]string{"submit", "--type", "event"})
	if err := root.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if f.submitted[1]["description"] != "a meetup\nin Berlin" || f.submitted[1]["type"] != "event" {
		t.Fatalf("sent %v", f.submitted[1])
	}
	if _, has := f.submitted[1]["url"]; has {
		t.Fatalf("url should be omitted: %v", f.submitted[1])
	}
}

func TestSubmitValidationHappensBeforeTheRequest(t *testing.T) {
	f := loggedIn(t)
	for name, args := range map[string][]string{
		"missing type": {"submit", "-d", "x"},
		"bad type":     {"submit", "-t", "video", "-d", "x"},
		"blank desc":   {"submit", "-t", "code", "-d", "   "},
	} {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if len(f.submitted) != 0 {
		t.Fatalf("nothing should reach the server: %v", f.submitted)
	}
}

func TestSubmitShowsServerMessage(t *testing.T) {
	loggedIn(t)
	_, err := run(t, "submit", "-t", "content", "-d", "over the limit")
	if err == nil || !strings.Contains(err.Error(), "maximum number of content pieces") || strings.Contains(err.Error(), "429") {
		t.Fatalf("error = %v", err)
	}
}

func runWithInput(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	out := &syncBuf{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetIn(strings.NewReader(input))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestDelete(t *testing.T) {
	f := loggedIn(t)

	// answering no (or nothing) cancels
	for _, in := range []string{"n\n", "\n", ""} {
		got, err := runWithInput(t, in, "delete", "cont-fresh")
		if err != nil || !strings.Contains(got, "Cancelled") {
			t.Fatalf("input %q: %v\n%s", in, err, got)
		}
	}
	if len(f.deleted) != 0 {
		t.Fatalf("deleted despite cancelling: %v", f.deleted)
	}

	// confirming deletes, and a fresh submission says the slot comes back
	got, err := runWithInput(t, "y\n", "delete", "cont-fresh")
	if err != nil || !strings.Contains(got, "gives the slot back") || !strings.Contains(got, "Deleted cont-fresh") {
		t.Fatalf("%v\n%s", err, got)
	}

	// an old one warns that the slot stays used; --yes skips the prompt
	got, err = runWithInput(t, "", "delete", "--yes", "cont-stale")
	if err != nil || !strings.Contains(got, "still count toward the monthly limit") || !strings.Contains(got, "Deleted cont-stale") {
		t.Fatalf("%v\n%s", err, got)
	}
	if fmt.Sprint(f.deleted) != "[cont-fresh cont-stale]" {
		t.Fatalf("deleted = %v", f.deleted)
	}
}

func TestDeleteUnknownID(t *testing.T) {
	f := loggedIn(t)
	_, err := runWithInput(t, "y\n", "delete", "nope")
	if err == nil || !strings.Contains(err.Error(), "does not exist") || len(f.deleted) != 0 {
		t.Fatalf("err = %v, deleted = %v", err, f.deleted)
	}
	if _, err := run(t, "delete"); err == nil {
		t.Fatal("delete without an id must fail")
	}
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func pngFile(t *testing.T, name string) string {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return writeTemp(t, name, b.Bytes())
}

func TestSubmitWithAttachments(t *testing.T) {
	f := loggedIn(t)
	img := pngFile(t, "badge.png")
	pdf := writeTemp(t, "ticket.pdf", []byte("%PDF-1.4\n%%EOF\n"))
	got, err := run(t, "submit", "-t", "event", "-d", "Berlin meetup", "-a", img, "--attach", pdf)
	if err != nil || !strings.Contains(got, "Submitted") {
		t.Fatalf("%v\n%s", err, got)
	}
	s := f.submitted[0]
	if s["type"] != "event" || s["description"] != "Berlin meetup" || s["files"] != "badge.png:image/png,ticket.pdf:application/pdf" {
		t.Fatalf("server received %v", s)
	}
}

func TestSubmitAttachmentChecksHappenLocally(t *testing.T) {
	f := loggedIn(t)
	good := pngFile(t, "ok.png")
	big := writeTemp(t, "big.png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, maxAttachmentBytes)...))
	txt := writeTemp(t, "notes.png", []byte("not really an image"))
	empty := writeTemp(t, "empty.png", nil)
	for name, args := range map[string][]string{
		"missing file": {"-a", "/nonexistent/file.png"},
		"directory":    {"-a", t.TempDir()},
		"too big":      {"-a", big},
		"wrong type":   {"-a", txt},
		"empty":        {"-a", empty},
		"four files":   {"-a", good, "-a", good, "-a", good, "-a", good},
	} {
		args = append([]string{"submit", "-t", "event", "-d", "x"}, args...)
		if _, err := run(t, args...); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if len(f.submitted) != 0 {
		t.Fatalf("nothing should reach the server: %v", f.submitted)
	}
}
