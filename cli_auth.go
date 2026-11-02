package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qdrant-labs/stars-submission-page/internals"
)

// CLI login piggybacks on the browser login: the CLI opens /auth/cli, the user
// logs in through the normal OIDC flow, and the callback hands a one-time code
// to the CLI's loopback server. The CLI trades the code (plus its PKCE verifier)
// for a session token, which it then sends as "Authorization: Bearer <token>".

const (
	cliSessionTTL = 30 * 24 * time.Hour
	cliCodeTTL    = 2 * time.Minute
)

// state and challenge end up in cookies and a redirect URL, so keep them to base64url.
var (
	cliStateRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	cliChallengeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`) // base64url(sha256)
)

type cliCode struct {
	sid       string
	challenge string
	expires   time.Time
}

// cliCodes holds pending one-time codes in memory; a restart just invalidates them.
type cliCodes struct {
	mu sync.Mutex
	m  map[string]cliCode
}

func (c *cliCodes) put(code string, v cliCode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cliCode{}
	}
	now := time.Now()
	for k, old := range c.m {
		if now.After(old.expires) {
			delete(c.m, k)
		}
	}
	c.m[code] = v
}

// take removes and returns the code, so each one works at most once.
func (c *cliCodes) take(code string) (cliCode, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[code]
	delete(c.m, code)
	return v, ok && time.Now().Before(v.expires)
}

func validCLIPort(s string) (int, bool) {
	p, err := strconv.Atoi(s)
	return p, err == nil && p >= 1024 && p <= 65535
}

// cliLogin is where the CLI sends the browser: GET /auth/cli?port=&state=&challenge=
func (a *App) cliLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, ok := validCLIPort(q.Get("port")); !ok {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	if !cliStateRe.MatchString(q.Get("state")) || !cliChallengeRe.MatchString(q.Get("challenge")) {
		http.Error(w, "invalid state or challenge", http.StatusBadRequest)
		return
	}
	a.setCookie(w, "cli_port", q.Get("port"), "/auth", 10*time.Minute)
	a.setCookie(w, "cli_state", q.Get("state"), "/auth", 10*time.Minute)
	a.setCookie(w, "cli_challenge", q.Get("challenge"), "/auth", 10*time.Minute)
	a.startLogin(w, r)
}

func (a *App) clearCLICookies(w http.ResponseWriter) {
	for _, name := range []string{"cli_port", "cli_state", "cli_challenge"} {
		a.setCookie(w, name, "", "/auth", -1)
	}
}

// finishCLILogin is called by the OIDC callback once the user is verified. If the
// login was started by the CLI it creates the session, redirects to the CLI's
// loopback server and returns true; otherwise it does nothing and returns false.
func (a *App) finishCLILogin(w http.ResponseWriter, r *http.Request, u internals.User) bool {
	pc, err1 := r.Cookie("cli_port")
	sc, err2 := r.Cookie("cli_state")
	cc, err3 := r.Cookie("cli_challenge")
	if err1 != nil || err2 != nil || err3 != nil || pc.Value == "" {
		return false
	}
	a.clearCLICookies(w)
	a.setCookie(w, "oidc_state", "", "/auth", -1)
	a.setCookie(w, "oidc_verifier", "", "/auth", -1)
	a.setCookie(w, "oidc_nonce", "", "/auth", -1)

	port, ok := validCLIPort(pc.Value)
	if !ok || !cliStateRe.MatchString(sc.Value) || !cliChallengeRe.MatchString(cc.Value) {
		http.Error(w, "invalid CLI login parameters", http.StatusBadRequest)
		return true
	}
	sid := randString(32)
	if err := a.store.PutSession(sid, u, cliSessionTTL); err != nil {
		log.Printf("could not store CLI session: %v", err)
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return true
	}
	code := randString(32)
	a.cli.put(code, cliCode{sid: sid, challenge: cc.Value, expires: time.Now().Add(cliCodeTTL)})
	http.Redirect(w, r, fmt.Sprintf("http://127.0.0.1:%d/callback?code=%s&state=%s", port, code, sc.Value), http.StatusFound)
	return true
}

// cliToken trades a one-time code and the PKCE verifier for the session token.
func (a *App) cliToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Code     string `json:"code"`
		Verifier string `json:"verifier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	c, ok := a.cli.take(req.Code)
	if !ok {
		http.Error(w, "invalid or expired code", http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256([]byte(req.Verifier))
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(c.challenge)) != 1 {
		a.store.DeleteSession(c.sid)
		http.Error(w, "invalid verifier", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"token": c.sid})
}

// sessionID returns the session ID from a bearer token (CLI) or the session cookie (browser).
func sessionID(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := strings.CutPrefix(h, "Bearer ")
		tok = strings.TrimSpace(tok)
		return tok, ok && tok != ""
	}
	if c, err := r.Cookie("session"); err == nil {
		return c.Value, true
	}
	return "", false
}
