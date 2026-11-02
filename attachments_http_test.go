package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qdrant-labs/stars-submission-page/internals"
)

type attachEnv struct {
	app   *App
	mux   *http.ServeMux
	dir   string
	queue chan internals.Submission
}

var (
	userAlice = internals.User{Sub: "alice", Iss: "i", Login: "alice", Email: "a@example.com"}
	userBob   = internals.User{Sub: "bob", Iss: "i", Login: "bob"}
	userAdmin = internals.User{Sub: "root", Iss: "i", Login: "root", Groups: []string{internals.AdminGroup}}
)

func newAttachEnv(t *testing.T) *attachEnv {
	t.Helper()
	a := newCLITestApp(t)
	dir := filepath.Join(t.TempDir(), "att")
	var err error
	if a.attachments, err = internals.NewAttachmentStore(dir); err != nil {
		t.Fatal(err)
	}
	queue := make(chan internals.Submission, 16)
	a.submissionsChannel = queue
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/submissions", a.require(a.submit))
	mux.HandleFunc("GET /api/submissions/{id}/attachments/{aid}", a.getAttachment)
	mux.HandleFunc("DELETE /api/submissions/{id}", a.require(a.deleteSubmission))
	return &attachEnv{a, mux, dir, queue}
}

func (e *attachEnv) do(t *testing.T, u *internals.User, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if u != nil {
		sid := "sid-" + u.Sub
		if err := e.app.store.PutSession(sid, *u, time.Hour); err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: "session", Value: sid})
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

type file struct {
	name string
	data []byte
}

func multipartBody(t *testing.T, data any, files ...file) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if data != nil {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="data"`)
		h.Set("Content-Type", "application/json")
		p, _ := w.CreatePart(h)
		json.NewEncoder(p).Encode(data)
	}
	for _, f := range files {
		p, err := w.CreateFormFile("files", f.name)
		if err != nil {
			t.Fatal(err)
		}
		p.Write(f.data)
	}
	w.Close()
	return buf.Bytes(), w.FormDataContentType()
}

func smallPNG(t *testing.T) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

var smallPDF = []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n%%EOF\n")

func fileCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func submitWith(t *testing.T, e *attachEnv, u *internals.User, typ string, files ...file) *httptest.ResponseRecorder {
	body, ct := multipartBody(t, map[string]string{"type": typ, "description": "I was there"}, files...)
	return e.do(t, u, "POST", "/api/submissions", ct, body)
}

func TestSubmitWithAttachments(t *testing.T) {
	e := newAttachEnv(t)
	rec := submitWith(t, e, &userAlice, "event", file{"badge.png", smallPNG(t)}, file{"ticket.pdf", smallPDF})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out internals.SubmissionResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	got, err := e.app.store.GetSubmission(out.ID, userAlice)
	if err != nil || len(got.Attachments) != 2 || got.Attachments[0].Name != "badge.png" || got.Attachments[1].ContentType != "application/pdf" {
		t.Fatalf("%+v, %v", got, err)
	}
	if fileCount(t, e.dir) != 2 {
		t.Fatalf("files on disk = %d", fileCount(t, e.dir))
	}
	// the Slack notification gets the attachments, as admin-only download links
	queued := <-e.queue
	links := attachmentLinks("https://stars.example", queued)
	if len(links) != 2 || links[0] != "https://stars.example/api/submissions/"+out.ID+"/attachments/"+got.Attachments[0].ID {
		t.Fatalf("links = %v", links)
	}
}

func TestSubmitWithoutAttachmentsStillWorks(t *testing.T) {
	e := newAttachEnv(t)
	body, _ := json.Marshal(map[string]string{"type": "content", "description": "post"})
	if rec := e.do(t, &userAlice, "POST", "/api/submissions", "application/json", body); rec.Code != http.StatusOK {
		t.Fatalf("json: %d %s", rec.Code, rec.Body)
	}
	if rec := submitWith(t, e, &userAlice, "content"); rec.Code != http.StatusOK {
		t.Fatalf("multipart without files: %d %s", rec.Code, rec.Body)
	}
	if links := attachmentLinks("https://x", internals.Submission{ID: "c-1"}); links == nil || len(links) != 0 {
		t.Fatalf("no attachments must give an empty, non-nil slice: %#v", links)
	}
}

func TestClientCannotClaimAttachments(t *testing.T) {
	e := newAttachEnv(t)
	body, _ := json.Marshal(map[string]any{
		"type": "event", "description": "x",
		"attachments": []map[string]any{{"id": "someone-elses-file", "name": "x.png", "content_type": "image/png", "size": 1}},
	})
	rec := e.do(t, &userAlice, "POST", "/api/submissions", "application/json", body)
	var out internals.SubmissionResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	got, err := e.app.store.GetSubmission(out.ID, userAlice)
	if err != nil || len(got.Attachments) != 0 {
		t.Fatalf("attachments = %+v, %v", got, err)
	}
	// same for the multipart "data" part
	mb, ct := multipartBody(t, map[string]any{"type": "event", "description": "x", "attachments": []map[string]any{{"id": "zzz"}}})
	rec = e.do(t, &userAlice, "POST", "/api/submissions", ct, mb)
	json.Unmarshal(rec.Body.Bytes(), &out)
	if got, _ := e.app.store.GetSubmission(out.ID, userAlice); len(got.Attachments) != 0 {
		t.Fatalf("multipart data claimed attachments: %+v", got.Attachments)
	}
}

func TestRejectedUploadsLeaveNothingBehind(t *testing.T) {
	e := newAttachEnv(t)
	big := append(smallPNG(t), make([]byte, internals.MaxAttachmentBytes)...)
	for name, tc := range map[string]struct {
		files []file
		want  int
	}{
		"wrong type":       {[]file{{"x.png", []byte("<script>1</script>")}}, http.StatusBadRequest},
		"too many":         {[]file{{"1.png", smallPNG(t)}, {"2.png", smallPNG(t)}, {"3.png", smallPNG(t)}, {"4.png", smallPNG(t)}}, http.StatusBadRequest},
		"over 2MB":         {[]file{{"big.png", big}}, http.StatusRequestEntityTooLarge},
		"one good one bad": {[]file{{"ok.png", smallPNG(t)}, {"bad.png", []byte("nope")}}, http.StatusBadRequest},
		"huge body":        {[]file{{"a.png", append(smallPNG(t), make([]byte, 9<<20)...)}}, http.StatusRequestEntityTooLarge},
	} {
		if rec := submitWith(t, e, &userAlice, "event", tc.files...); rec.Code != tc.want {
			t.Errorf("%s: status %d (%s), want %d", name, rec.Code, strings.TrimSpace(rec.Body.String()), tc.want)
		}
	}
	if fileCount(t, e.dir) != 0 {
		t.Fatalf("%d orphan files", fileCount(t, e.dir))
	}
	if subs, _ := e.app.store.GetSubmissionsByUser(userAlice); len(subs) != 0 {
		t.Fatalf("rejected uploads must not create submissions: %+v", subs)
	}

	// a missing data part is invalid too
	mb, ct := multipartBody(t, nil, file{"a.png", smallPNG(t)})
	if rec := e.do(t, &userAlice, "POST", "/api/submissions", ct, mb); rec.Code != http.StatusBadRequest {
		t.Fatalf("no data part: %d", rec.Code)
	}
}

func TestFailedSubmissionRemovesItsFiles(t *testing.T) {
	e := newAttachEnv(t)
	for i := 0; i < internals.MaxContentPieces; i++ {
		if rec := submitWith(t, e, &userAlice, "content"); rec.Code != http.StatusOK {
			t.Fatal(rec.Code)
		}
	}
	rec := submitWith(t, e, &userAlice, "content", file{"x.png", smallPNG(t)})
	if rec.Code != http.StatusTooManyRequests || fileCount(t, e.dir) != 0 {
		t.Fatalf("status %d, files %d", rec.Code, fileCount(t, e.dir))
	}
	// an invalid description is rejected after the files were stored, and cleaned up
	mb, ct := multipartBody(t, map[string]string{"type": "event", "description": "  "}, file{"x.png", smallPNG(t)})
	if rec := e.do(t, &userAlice, "POST", "/api/submissions", ct, mb); rec.Code != http.StatusBadRequest || fileCount(t, e.dir) != 0 {
		t.Fatalf("status %d, files %d", rec.Code, fileCount(t, e.dir))
	}
}

func TestPerUserAttachmentCap(t *testing.T) {
	e := newAttachEnv(t)
	pre := internals.Submission{SubmissionType: "event", Details: "old",
		Attachments: []internals.Attachment{{ID: "old", Name: "o.png", ContentType: "image/png", Size: internals.MaxUserAttachmentBytes - 10}}}
	if _, err := e.app.store.PutSubmission(userAlice, pre); err != nil {
		t.Fatal(err)
	}
	if rec := submitWith(t, e, &userAlice, "event", file{"x.png", smallPNG(t)}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the per-user cap: %d %s", rec.Code, rec.Body)
	}
	// another user is unaffected
	if rec := submitWith(t, e, &userBob, "event", file{"x.png", smallPNG(t)}); rec.Code != http.StatusOK {
		t.Fatalf("bob: %d", rec.Code)
	}
}

func TestOnlyAdminsCanDownload(t *testing.T) {
	e := newAttachEnv(t)
	data := smallPNG(t)
	rec := submitWith(t, e, &userAlice, "event", file{`..\evil "name".png`, data})
	var out internals.SubmissionResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	sub, _ := e.app.store.GetSubmission(out.ID, userAlice)
	path := "/api/submissions/" + out.ID + "/attachments/" + sub.Attachments[0].ID

	if rec := e.do(t, &userAdmin, "GET", path, "", nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("admin: %d", rec.Code)
	} else {
		h := rec.Header()
		if h.Get("Content-Type") != "image/png" || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment;") ||
			h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") ||
			!strings.Contains(h.Get("Cache-Control"), "no-store") {
			t.Fatalf("headers: %v", h)
		}
	}
	// not even the owner
	for name, u := range map[string]*internals.User{"owner": &userAlice, "other user": &userBob} {
		if rec := e.do(t, u, "GET", path, "", nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, rec.Code)
		}
	}
	// no session: sent to log in, then back to the same file
	rec = e.do(t, nil, "GET", path, "", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login?next=%2Fapi%2Fsubmissions%2F"+out.ID+"%2Fattachments%2F"+sub.Attachments[0].ID {
		t.Fatalf("anonymous: %d -> %s", rec.Code, rec.Header().Get("Location"))
	}
	// that next value is accepted by login, and the cookie holds it
	if !attachmentPathRe.MatchString(path) {
		t.Fatal("download path must be an allowed login destination")
	}
	// unknown ids
	if rec := e.do(t, &userAdmin, "GET", "/api/submissions/"+out.ID+"/attachments/nope", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown attachment: %d", rec.Code)
	}
	if rec := e.do(t, &userAdmin, "GET", "/api/submissions/nope/attachments/x", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown submission: %d", rec.Code)
	}
}

func TestDeleteRemovesAttachmentFiles(t *testing.T) {
	for name, deleter := range map[string]*internals.User{"owner": &userAlice, "admin": &userAdmin} {
		e := newAttachEnv(t)
		rec := submitWith(t, e, &userAlice, "event", file{"a.png", smallPNG(t)}, file{"b.pdf", smallPDF})
		var out internals.SubmissionResponse
		json.Unmarshal(rec.Body.Bytes(), &out)
		if fileCount(t, e.dir) != 2 {
			t.Fatal("setup")
		}
		if rec := e.do(t, &userBob, "DELETE", "/api/submissions/"+out.ID, "", nil); rec.Code != http.StatusForbidden || fileCount(t, e.dir) != 2 {
			t.Fatalf("someone else deleting: %d, files %d", rec.Code, fileCount(t, e.dir))
		}
		if rec := e.do(t, deleter, "DELETE", "/api/submissions/"+out.ID, "", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: %d", name, rec.Code)
		}
		if fileCount(t, e.dir) != 0 {
			t.Fatalf("%s delete left %d files", name, fileCount(t, e.dir))
		}
		if rec := e.do(t, deleter, "DELETE", "/api/submissions/"+out.ID, "", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("second delete: %d", rec.Code)
		}
	}
}

func TestLoginNextIsRestrictedToAttachmentLinks(t *testing.T) {
	e := newAttachEnv(t)
	for next, want := range map[string]bool{
		"/api/submissions/ev-abc_1/attachments/Xy-9":   true,
		"https://evil.example/":                        false,
		"//evil.example/":                              false,
		"/api/submissions/ev-abc/attachments/../../me": false,
		"/api/submissions/ev-abc/attachments/x?y=1":    false,
		"/api/me": false,
		"/api/submissions/ev-abc/attachments/x/../../..": false,
	} {
		rec := httptest.NewRecorder()
		e.app.login(rec, httptest.NewRequest("GET", "/auth/login?next="+urlEscape(next), nil))
		got := false
		for _, c := range rec.Result().Cookies() {
			if c.Name == "login_next" && c.Value != "" && c.MaxAge >= 0 {
				got = c.Value == next
			}
		}
		if got != want {
			t.Errorf("next %q: remembered=%v, want %v", next, got, want)
		}
	}
}

func urlEscape(s string) string { return strings.NewReplacer("?", "%3F", "&", "%26").Replace(s) }
