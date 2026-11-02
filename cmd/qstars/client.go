package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// errUnauthorized means the saved token is no longer valid.
type errUnauthorized struct{}

func (errUnauthorized) Error() string {
	return "your session has expired or was revoked: run `qstars login` again"
}

// apiError is an error response from the server. Its text is the server's own
// message when there is one, since those are written for users (e.g. the monthly limit).
type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("server returned %d %s", e.Status, http.StatusText(e.Status))
}

type client struct {
	server string
	token  string
	http   *http.Client
}

func newClient(server, token string) *client {
	return &client{server: strings.TrimRight(server, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// do sends the request with the bearer token and decodes a JSON response into out (if non-nil).
func (c *client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, body)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return errUnauthorized{}
	}
	if res.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return &apiError{Status: res.StatusCode, Msg: strings.TrimSpace(string(msg))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// user is the subset of /api/me the CLI shows.
type user struct {
	Name   string   `json:"name"`
	Login  string   `json:"preferred_username"`
	Email  string   `json:"email"`
	Groups []string `json:"groups"`
}

func (u user) display() string {
	name := u.Login
	if name == "" {
		name = u.Name
	}
	if u.Email != "" {
		return fmt.Sprintf("%s <%s>", name, u.Email)
	}
	return name
}

type submission struct {
	ID            string `json:"id"`
	OwnerUsername string `json:"owner_username"`
	OwnerEmail    string `json:"owner_email"`
	Type          string `json:"type"`
	URL           string `json:"url"`
	Description   string `json:"description"`
	Timestamp     string `json:"timestamp"`
}
