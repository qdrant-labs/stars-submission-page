package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out and forget the saved session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := loadConfig()
			if errors.Is(err, errNotLoggedIn) {
				fmt.Fprintln(cmd.OutOrStdout(), "You are not logged in.")
				return nil
			}
			if err != nil {
				return err
			}
			// Revoke the session on the server first; the local file goes either way.
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			revokeErr := newClient(c.Server, c.Token).do(ctx, http.MethodPost, "/auth/logout", nil, nil)
			if err := deleteConfig(); err != nil {
				return err
			}
			if revokeErr != nil && !errors.As(revokeErr, new(errUnauthorized)) {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not revoke the session on the server (%v); it will expire on its own.\n", revokeErr)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Logged out.")
			return nil
		},
	}
}
