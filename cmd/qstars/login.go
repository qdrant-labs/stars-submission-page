package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const loginTimeout = 5 * time.Minute

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newLoginCmd() *cobra.Command {
	var server string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in through your browser (the same login as the website)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if server == "" {
				server = os.Getenv("QSTARS_URL")
			}
			if server == "" {
				if c, err := loadConfig(); err == nil {
					server = c.Server
				}
			}
			if server == "" {
				return errors.New("no server: pass --server <url> or set QSTARS_URL")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), loginTimeout)
			defer cancel()
			return runLogin(ctx, cmd, strings.TrimRight(server, "/"), noBrowser)
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "base URL of the submission page (default: $QSTARS_URL or the last server used)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "only print the login URL instead of opening a browser")
	return cmd
}

type callbackResult struct {
	code string
	err  error
}

func runLogin(ctx context.Context, cmd *cobra.Command, server string, noBrowser bool) error {
	verifier, err := randomString(32)
	if err != nil {
		return err
	}
	state, err := randomString(16)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// Loopback server that receives the one-time code from the website.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("could not start the local callback server: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	results := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || q.Get("code") == "" {
			http.Error(w, "invalid login response", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>qstars</title><p>You are logged in. You can close this tab and go back to your terminal.</p>")
		select {
		case results <- callbackResult{code: q.Get("code")}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	loginURL := fmt.Sprintf("%s/auth/cli?%s", server, url.Values{
		"port":      {fmt.Sprint(port)},
		"state":     {state},
		"challenge": {challenge},
	}.Encode())

	out := cmd.OutOrStdout()
	if noBrowser {
		fmt.Fprintf(out, "Open this URL in a browser on this machine to log in:\n\n  %s\n\n", loginURL)
	} else if err := openBrowser(loginURL); err != nil {
		fmt.Fprintf(out, "Could not open a browser (%v). Open this URL to log in:\n\n  %s\n\n", err, loginURL)
	} else {
		fmt.Fprintf(out, "Opening your browser to log in. If nothing opens, go to:\n\n  %s\n\n", loginURL)
	}
	fmt.Fprintln(out, "Waiting for you to finish logging in…")

	var res callbackResult
	select {
	case res = <-results:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("timed out waiting for the login to finish")
		}
		return ctx.Err()
	}

	token, err := exchangeCode(ctx, server, res.code, verifier)
	if err != nil {
		return err
	}
	c := newClient(server, token)
	var me user
	if err := c.do(ctx, http.MethodGet, "/api/me", nil, &me); err != nil {
		return fmt.Errorf("logged in, but could not verify the session: %w", err)
	}
	if err := saveConfig(&config{Server: server, Token: token}); err != nil {
		return fmt.Errorf("logged in, but could not save the session: %w", err)
	}
	fmt.Fprintf(out, "Logged in as %s.\n", me.display())
	return nil
}

func exchangeCode(ctx context.Context, server, code, verifier string) (string, error) {
	body, err := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"token"`
	}
	c := newClient(server, "")
	if err := c.do(ctx, http.MethodPost, "/auth/cli/token", bytes.NewReader(body), &out); err != nil {
		return "", fmt.Errorf("could not complete the login: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("the server returned no token")
	}
	return out.Token, nil
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
