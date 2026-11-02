package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var submissionTypes = []string{"content", "code", "event", "other"}

func newSubmitCmd() *cobra.Command {
	var typ, link, description string
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Submit a content piece, code, event or other item for review",
		Long: `Submit something for review.

The description comes from --description, or from standard input when it is piped
in (or when --description is "-"):

  qstars submit -t content -u https://example.com/post -d "My post about hybrid search"
  qstars submit -t event < description.txt`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains(submissionTypes, typ) {
				return fmt.Errorf("invalid --type %q: must be one of %s", typ, strings.Join(submissionTypes, ", "))
			}
			desc, err := readDescription(cmd, description)
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
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			var out struct {
				ID string `json:"id"`
			}
			if err := newClient(cfg.Server, cfg.Token).do(ctx, http.MethodPost, "/api/submissions", bytes.NewReader(body), &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Submitted %s. Thank you!\n", out.ID)
			return nil
		},
	}
	cmd.Flags().StringVarP(&typ, "type", "t", "", "what you are submitting: "+strings.Join(submissionTypes, ", ")+" (required)")
	cmd.Flags().StringVarP(&link, "url", "u", "", "link to the content or event (optional)")
	cmd.Flags().StringVarP(&description, "description", "d", "", `description; "-" or piped input reads it from stdin`)
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
