package internals

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pngBytes(t testing.TB) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegBytes(t testing.TB) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

var pdfBytes = []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")

func newTestAttachments(t *testing.T) (*AttachmentStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "att")
	s, err := NewAttachmentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func filesIn(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestSaveAndOpen(t *testing.T) {
	s, dir := newTestAttachments(t)
	atts, err := s.Save([]Upload{{"proof.png", pngBytes(t)}, {"photo.jpg", jpegBytes(t)}, {"ticket.pdf", pdfBytes}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"image/png", "image/jpeg", "application/pdf"}
	for i, a := range atts {
		if a.ContentType != want[i] || a.Size == 0 || a.ID == "" {
			t.Fatalf("attachment %d = %+v", i, a)
		}
	}
	if filesIn(t, dir) != 3 {
		t.Fatalf("want 3 files on disk")
	}
	f, err := s.Open(atts[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got, _ := io.ReadAll(f); !bytes.Equal(got, pdfBytes) {
		t.Fatal("stored bytes differ")
	}
	if st, _ := os.Stat(filepath.Join(dir, atts[0].ID)); st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", st.Mode().Perm())
	}
}

func TestTypeComesFromContentNotName(t *testing.T) {
	s, dir := newTestAttachments(t)
	// a script and an HTML page dressed up as images, and an SVG
	for name, data := range map[string][]byte{
		"evil.png": []byte("<script>alert(1)</script>"),
		"page.jpg": []byte("<!DOCTYPE html><html><body>hi</body></html>"),
		"pic.png":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
		"text.pdf": []byte("just some text"),
	} {
		if _, err := s.Save([]Upload{{name, data}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	// and a real PNG is accepted whatever it is called
	if _, err := s.Save([]Upload{{"image.txt", pngBytes(t)}}); err != nil {
		t.Fatal(err)
	}
	if filesIn(t, dir) != 1 {
		t.Fatal("rejected uploads must not leave files")
	}
}

func TestSizeAndCountLimits(t *testing.T) {
	s, dir := newTestAttachments(t)
	big := append(pngBytes(t), make([]byte, MaxAttachmentBytes)...)
	if _, err := s.Save([]Upload{{"big.png", big}}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too big: %v", err)
	}
	exact := append(pngBytes(t), make([]byte, MaxAttachmentBytes-int64(len(pngBytes(t))))...)
	if int64(len(exact)) != MaxAttachmentBytes {
		t.Fatal("test setup")
	}
	if _, err := s.Save([]Upload{{"exact.png", exact}}); err != nil {
		t.Fatalf("exactly at the limit: %v", err)
	}
	if _, err := s.Save([]Upload{{"empty.png", nil}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty: %v", err)
	}
	many := make([]Upload, MaxAttachmentsPerSubmission+1)
	for i := range many {
		many[i] = Upload{"a.png", pngBytes(t)}
	}
	if _, err := s.Save(many); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many: %v", err)
	}
	if filesIn(t, dir) != 1 {
		t.Fatalf("only the one valid upload should be on disk, found %d", filesIn(t, dir))
	}
}

func TestSaveIsAllOrNothing(t *testing.T) {
	s, dir := newTestAttachments(t)
	_, err := s.Save([]Upload{{"ok.png", pngBytes(t)}, {"bad.png", []byte("nope")}})
	if err == nil || filesIn(t, dir) != 0 {
		t.Fatalf("err = %v, files = %d", err, filesIn(t, dir))
	}
}

func TestNamesAreCleaned(t *testing.T) {
	for in, want := range map[string]string{
		"proof.png":              "proof.png",
		"../../etc/passwd":       "passwd",
		`C:\Users\me\scan.pdf`:   "scan.pdf",
		"a\r\nb.png":             "ab.png",
		"":                       "attachment",
		"..":                     "attachment",
		"   ":                    "attachment",
		strings.Repeat("é", 300): strings.Repeat("é", 100),
	} {
		if got := cleanAttachmentName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestOpenRejectsPathTraversal(t *testing.T) {
	s, dir := newTestAttachments(t)
	secret := filepath.Join(filepath.Dir(dir), "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../secret", "..", "a/b", "", strings.Repeat("a", 65), "missing"} {
		if f, err := s.Open(id); !errors.Is(err, ErrNotFound) {
			if f != nil {
				f.Close()
			}
			t.Errorf("Open(%q) err = %v, want ErrNotFound", id, err)
		}
	}
}

func TestRemove(t *testing.T) {
	s, dir := newTestAttachments(t)
	atts, _ := s.Save([]Upload{{"a.png", pngBytes(t)}, {"b.png", pngBytes(t)}})
	s.Remove(atts...)
	s.Remove(atts...) // already gone: no panic
	s.Remove(Attachment{ID: "../x"})
	if filesIn(t, dir) != 0 {
		t.Fatal("files remain")
	}
}

func TestAttachmentsPersistAndCount(t *testing.T) {
	st := newTestStore(t)
	sub := content()
	sub.Attachments = []Attachment{{ID: "a", Name: "x.png", ContentType: "image/png", Size: 100}, {ID: "b", Name: "y.pdf", ContentType: "application/pdf", Size: 250}}
	id, err := st.PutSubmission(alice, sub)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSubmission(id, alice)
	if err != nil || len(got.Attachments) != 2 || got.Attachments[1].Name != "y.pdf" {
		t.Fatalf("%+v, %v", got, err)
	}
	if n, err := st.AttachmentBytesUsed(alice); err != nil || n != 350 {
		t.Fatalf("used = %d, %v", n, err)
	}
	if n, _ := st.AttachmentBytesUsed(bob); n != 0 {
		t.Fatalf("bob used = %d", n)
	}
	if err := st.DeleteSubmission(id, alice); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.AttachmentBytesUsed(alice); n != 0 {
		t.Fatalf("after delete: %d", n)
	}
}
