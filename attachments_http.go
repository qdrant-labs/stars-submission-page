package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"

	"github.com/qdrant-labs/stars-submission-page/internals"
)

// A multipart submission is the JSON part plus up to MaxAttachmentsPerSubmission files.
const maxMultipartBytes = maxBodyBytes + internals.MaxAttachmentsPerSubmission*internals.MaxAttachmentBytes + 64<<10

// attachmentPathRe is the only place a login may send the user back to (see login/callback).
var attachmentPathRe = regexp.MustCompile(`^/api/submissions/[A-Za-z0-9_-]{1,100}/attachments/[A-Za-z0-9_-]{1,64}$`)

// readSubmissionRequest reads either a JSON body (no files) or a multipart body with a
// "data" part (the same JSON) and any number of "files" parts.
func readSubmissionRequest(w http.ResponseWriter, r *http.Request) (internals.Submission, []internals.Upload, error) {
	var req internals.Submission
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
			return req, nil, fmt.Errorf("%w: malformed request body", internals.ErrInvalid)
		}
		return req, nil, nil
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxMultipartBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		return req, nil, fmt.Errorf("%w: malformed multipart body", internals.ErrInvalid)
	}
	var uploads []internals.Upload
	haveData := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return req, nil, bodyError(err)
		}
		switch part.FormName() {
		case "data":
			data, err := io.ReadAll(io.LimitReader(part, maxBodyBytes+1))
			if err != nil {
				return req, nil, bodyError(err)
			}
			if len(data) > maxBodyBytes || json.Unmarshal(data, &req) != nil {
				return req, nil, fmt.Errorf("%w: malformed submission data", internals.ErrInvalid)
			}
			haveData = true
		case "files":
			if len(uploads) >= internals.MaxAttachmentsPerSubmission {
				return req, nil, fmt.Errorf("%w: at most %d attachments per submission", internals.ErrInvalid, internals.MaxAttachmentsPerSubmission)
			}
			data, err := io.ReadAll(io.LimitReader(part, internals.MaxAttachmentBytes+1))
			if err != nil {
				return req, nil, bodyError(err)
			}
			uploads = append(uploads, internals.Upload{Name: part.FileName(), Data: data})
		}
	}
	if !haveData {
		return req, nil, fmt.Errorf("%w: the \"data\" part is missing", internals.ErrInvalid)
	}
	return req, uploads, nil
}

func bodyError(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return fmt.Errorf("%w: the upload is too large", internals.ErrTooLarge)
	}
	return fmt.Errorf("%w: could not read the upload", internals.ErrInvalid)
}

func (a *App) submit(w http.ResponseWriter, r *http.Request) {
	u, _ := a.userFrom(r)
	req, uploads, err := readSubmissionRequest(w, r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	req.Attachments = nil // clients cannot claim attachments; only stored uploads count

	if len(uploads) > 0 {
		var incoming int64
		for _, up := range uploads {
			incoming += int64(len(up.Data))
		}
		used, err := a.store.AttachmentBytesUsed(u)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if used+incoming > internals.MaxUserAttachmentBytes {
			writeStoreError(w, fmt.Errorf("%w: you reached the %d MB limit for stored attachments", internals.ErrTooLarge, internals.MaxUserAttachmentBytes>>20))
			return
		}
		if req.Attachments, err = a.attachments.Save(uploads); err != nil {
			writeStoreError(w, err)
			return
		}
	}

	submissionId, err := a.store.PutSubmission(u, req)
	if err != nil {
		a.attachments.Remove(req.Attachments...) // do not leave orphan files behind
		writeStoreError(w, err)
		return
	}
	a.notify(submissionId, u)
	writeJSON(w, internals.SubmissionResponse{ID: submissionId})
}

// getAttachment serves a stored file. Only admins may read attachments.
func (a *App) getAttachment(w http.ResponseWriter, r *http.Request) {
	u, ok := a.userFrom(r)
	if !ok {
		// Opened from a Slack link without a session: log in, then come back here.
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.Path), http.StatusFound)
		return
	}
	if !u.IsAdmin() {
		http.Error(w, "only admins can view attachments", http.StatusForbidden)
		return
	}
	sub, err := a.store.GetSubmission(r.PathValue("id"), u)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	i := slices.IndexFunc(sub.Attachments, func(at internals.Attachment) bool { return at.ID == r.PathValue("aid") })
	if i < 0 {
		http.Error(w, "attachment does not exist", http.StatusNotFound)
		return
	}
	att := sub.Attachments[i]
	f, err := a.attachments.Open(att.ID)
	if err != nil {
		if !errors.Is(err, internals.ErrNotFound) {
			log.Printf("could not open attachment %s: %v", att.ID, err)
		}
		writeStoreError(w, err)
		return
	}
	defer f.Close()

	// User-supplied content: force a download and forbid sniffing it as anything else.
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": att.Name})
	if disposition == "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", sub.Timestamp, f)
}
