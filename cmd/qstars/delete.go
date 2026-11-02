package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// refundWindow mirrors the server's rule; the server makes the actual decision.
const refundWindow = 24 * time.Hour

func newDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete one of your submissions (admins can delete any)",
		Long: `Delete a submission by its ID (see "qstars list").

Deleting within 24 hours of submitting gives the quota slot back; after that the
slot stays used. You are asked to confirm unless you pass --yes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			id := args[0]
			path := "/api/submissions/" + url.PathEscape(id)
			c := newClient(cfg.Server, cfg.Token)
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()

			var sub submission
			if err := c.do(ctx, http.MethodGet, path, nil, &sub); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s  %s  %s\n  %s\n  %s\n", sub.ID, sub.Type, shortDate(sub.Timestamp), orDash(sub.URL), oneLine(sub.Description, 100))
			if refundsSlot(sub) {
				fmt.Fprintln(out, "It was submitted less than 24 hours ago, so deleting it gives the slot back.")
			} else {
				fmt.Fprintln(out, "It will still count toward the monthly limit.")
			}

			if !yes && !confirm(cmd, "Delete this submission? This cannot be undone.") {
				fmt.Fprintln(out, "Cancelled.")
				return nil
			}
			if err := c.do(ctx, http.MethodDelete, path, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(out, "Deleted %s.\n", sub.ID)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func refundsSlot(s submission) bool {
	t, err := time.Parse(time.RFC3339Nano, s.Timestamp)
	return err == nil && time.Since(t) <= refundWindow
}

// confirm asks a yes/no question on stdin; anything but y/yes (or EOF) is a no.
func confirm(cmd *cobra.Command, prompt string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", prompt)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	fmt.Fprintln(cmd.OutOrStdout())
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
