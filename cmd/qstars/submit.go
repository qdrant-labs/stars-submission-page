package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var submissionTypes = []string{"content", "code", "event", "other"}

// Same limits as the server; checking here just gives a faster, clearer error.
const (
	maxAttachments     = 3
	maxAttachmentBytes = 2 << 20
)

type attachment struct {
	name string
	data []byte
}

func newSubmitCmd() *cobra.Command {
	var typ, link, description string
	var attach []string
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Submit a content piece, code, event or other item for review",
		Long: `Submit something for review.

The description comes from --description, or from standard input when it is piped
in (or when --description is "-"):

  qstars submit -t content -u https://example.com/post -d "My post about hybrid search"
  qstars submit -t event < description.txt
  qstars submit -t event -d "Qdrant meetup Berlin" -a badge.png -a ticket.pdf

Attachments (--attach, up to 3 PNG, JPG or PDF files of at most 2 MB each) are
for proof such as event attendance. Only admins can see them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains(submissionTypes, typ) {
				return fmt.Errorf("invalid --type %q: must be one of %s", typ, strings.Join(submissionTypes, ", "))
			}
			desc, err := readDescription(cmd, description)
			if err != nil {
				return err
			}
			files, err := readAttachments(attach)
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			req := map[string]string{"type": typ, "description": desc}
			if link = strings.TrimSpace(link); link != "" {
				req["url"] = link
			}
			body, err := json.Marshal(req)
			if err != nil {
				return err
			}
			contentType := "application/json"
			if len(files) > 0 {
				if body, contentType, err = multipartBody(body, files); err != nil {
					return err
				}
			}
			// uploads can take longer than a plain JSON request
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			var out struct {
				ID string `json:"id"`
			}
			if err := newClient(cfg.Server, cfg.Token).doAs(ctx, http.MethodPost, "/api/submissions", contentType, bytes.NewReader(body), &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Submitted %s. Thank you!\n", out.ID)
			return nil
		},
	}
	cmd.Flags().StringVarP(&typ, "type", "t", "", "what you are submitting: "+strings.Join(submissionTypes, ", ")+" (required)")
	cmd.Flags().StringVarP(&link, "url", "u", "", "link to the content or event (optional)")
	cmd.Flags().StringVarP(&description, "description", "d", "", `description; "-" or piped input reads it from stdin`)
	cmd.Flags().StringArrayVarP(&attach, "attach", "a", nil, "file to attach, e.g. proof of attendance (repeatable, up to 3)")
	cmd.MarkFlagRequired("type")
	return cmd
}

// readDescription takes the --description value, or stdin when the flag is "-"
// or absent and input is piped. It refuses to wait on an interactive terminal.
func readDescription(cmd *cobra.Command, flag string) (string, error) {
	desc := flag
	if flag == "" || flag == "-" {
		in := cmd.InOrStdin()
		if f, ok := in.(*os.File); ok {
			if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
				return "", errors.New(`no description: pass --description "...", or pipe it in on stdin`)
			}
		}
		b, err := io.ReadAll(io.LimitReader(in, 64*1024))
		if err != nil {
			return "", fmt.Errorf("could not read the description from stdin: %w", err)
		}
		desc = string(b)
	}
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return "", errors.New("the description is empty")
	}
	return desc, nil
}

// readAttachments loads and checks the files before anything is sent.
func readAttachments(paths []string) ([]attachment, error) {
	if len(paths) > maxAttachments {
		return nil, fmt.Errorf("at most %d attachments are allowed, got %d", maxAttachments, len(paths))
	}
	var files []attachment
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", p, err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("attachment %s is not a regular file", p)
		}
		if st.Size() > maxAttachmentBytes {
			return nil, fmt.Errorf("attachment %s is larger than %d MB", p, maxAttachmentBytes>>20)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", p, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("attachment %s is empty", p)
		}
		switch http.DetectContentType(data) {
		case "image/png", "image/jpeg", "application/pdf":
		default:
			return nil, fmt.Errorf("attachment %s must be a PNG, JPG or PDF file", p)
		}
		files = append(files, attachment{name: filepath.Base(p), data: data})
	}
	return files, nil
}

// multipartBody builds the upload: the submission JSON as "data", then each file as "files".
func multipartBody(data []byte, files []attachment) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="data"`)
	h.Set("Content-Type", "application/json")
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", err
	}
	for _, f := range files {
		part, err := w.CreateFormFile("files", f.name)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(f.data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}
