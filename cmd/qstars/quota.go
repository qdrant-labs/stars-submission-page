package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

func newQuotaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "quota",
		Short: "Show how many submissions you have left this month",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			var q struct {
				Content int `json:"content"`
				Code    int `json:"code"`
			}
			if err := newClient(cfg.Server, cfg.Token).do(ctx, http.MethodGet, "/api/quota", nil, &q); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "content: %d left this month\n", q.Content)
			fmt.Fprintf(out, "code:    %d left this month\n", q.Code)
			fmt.Fprintln(out, "Events and other submissions are unlimited. Deleting a submission within 24 hours gives its slot back.")
			return nil
		},
	}
}
