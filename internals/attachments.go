package internals

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	gonanoid "github.com/matoous/go-nanoid/v2"
)

const (
	MaxAttachmentBytes          int64 = 2 << 20  // per file
	MaxAttachmentsPerSubmission       = 3        // files per submission
	MaxUserAttachmentBytes      int64 = 50 << 20 // all of a user's stored attachments
	maxAttachmentNameRunes            = 100
)

// ErrTooLarge is returned for files (or total uploads) over the size limits.
var ErrTooLarge = errors.New("attachment too large")

// Attachment is the metadata kept on a submission. The bytes live on disk.
type Attachment struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// Upload is a file received from a client that has not been stored yet.
type Upload struct {
	Name string
	Data []byte
}

// attachmentIDRe is what gonanoid produces; checking it keeps IDs from escaping the directory.
var attachmentIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// AttachmentStore keeps attachment files in one directory (put it on the volume).
type AttachmentStore struct {
	dir string
}

func NewAttachmentStore(dir string) (*AttachmentStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("could not create the attachments directory: %w", err)
	}
	return &AttachmentStore{dir: dir}, nil
}

// detectAttachmentType trusts the file's content, never its name or the client's claim.
func detectAttachmentType(data []byte) (string, bool) {
	switch ct := http.DetectContentType(data); ct {
	case "image/png", "image/jpeg", "application/pdf":
		return ct, true
	}
	return "", false
}

// cleanAttachmentName keeps only the base name and drops control characters.
func cleanAttachmentName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > maxAttachmentNameRunes {
		name = string(r[:maxAttachmentNameRunes])
	}
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return name
}

// Save validates and stores the uploads. Either all of them are stored or none.
func (s *AttachmentStore) Save(uploads []Upload) ([]Attachment, error) {
	if len(uploads) > MaxAttachmentsPerSubmission {
		return nil, fmt.Errorf("%w: at most %d attachments per submission", ErrInvalid, MaxAttachmentsPerSubmission)
	}
	type checked struct {
		Upload
		contentType string
	}
	var ok []checked
	for _, u := range uploads {
		name := cleanAttachmentName(u.Name)
		if len(u.Data) == 0 {
			return nil, fmt.Errorf("%w: attachment %q is empty", ErrInvalid, name)
		}
		if int64(len(u.Data)) > MaxAttachmentBytes {
			return nil, fmt.Errorf("%w: attachment %q is larger than %d MB", ErrTooLarge, name, MaxAttachmentBytes>>20)
		}
		ct, good := detectAttachmentType(u.Data)
		if !good {
			return nil, fmt.Errorf("%w: attachment %q must be a PNG, JPG or PDF file", ErrInvalid, name)
		}
		ok = append(ok, checked{Upload{name, u.Data}, ct})
	}

	saved := make([]Attachment, 0, len(ok))
	for _, c := range ok {
		id, err := gonanoid.New()
		if err != nil {
			s.Remove(saved...)
			return nil, fmt.Errorf("error while creating ID for attachment: %w", err)
		}
		// O_EXCL: never overwrite an existing file.
		f, err := os.OpenFile(filepath.Join(s.dir, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = f.Write(c.Data)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				os.Remove(filepath.Join(s.dir, id))
			}
		}
		if err != nil {
			s.Remove(saved...)
			return nil, fmt.Errorf("error while storing attachment: %w", err)
		}
		saved = append(saved, Attachment{ID: id, Name: c.Name, ContentType: c.contentType, Size: int64(len(c.Data))})
	}
	return saved, nil
}

// Open returns the stored file; the caller must close it.
func (s *AttachmentStore) Open(id string) (*os.File, error) {
	if !attachmentIDRe.MatchString(id) {
		return nil, fmt.Errorf("%w: attachment", ErrNotFound)
	}
	f, err := os.Open(filepath.Join(s.dir, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: attachment %s", ErrNotFound, id)
	}
	return f, err
}

// Remove deletes the files, ignoring any that are already gone.
func (s *AttachmentStore) Remove(atts ...Attachment) {
	for _, a := range atts {
		if attachmentIDRe.MatchString(a.ID) {
			os.Remove(filepath.Join(s.dir, a.ID))
		}
	}
}

// AttachmentBytesUsed is the total size of the attachments on the user's stored submissions.
func (s *Store) AttachmentBytesUsed(user User) (int64, error) {
	subs, err := s.GetSubmissionsByUser(user)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, sub := range subs {
		for _, a := range sub.Attachments {
			total += a.Size
		}
	}
	return total, nil
}
