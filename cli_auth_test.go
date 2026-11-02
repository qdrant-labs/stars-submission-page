package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qdrant-labs/stars-submission-page/internals"
	"golang.org/x/oauth2"
)

func newCLITestApp(t *testing.T) *App {
	t.Helper()
	store, err := internals.NewStoreAt(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &App{
		store: store,
		oauth: &oauth2.Config{ClientID: "id", Endpoint: oauth2.Endpoint{AuthURL: "https://idp.example/authorize"}},
	}
}

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func cookiesOf(rec *httptest.ResponseRecorder) map[string]string {
	out := map[string]string{}
	for _, c := range rec.Result().Cookies() {
		out[c.Name] = c.Value
	}
	return out
}

func TestCLILoginValidatesParams(t *testing.T) {
	a := newCLITestApp(t)
	ch := challengeFor("v")
	for name, q := range map[string]string{
		"port too low":  "port=80&state=abc&challenge=" + ch,
		"port not int":  "port=x&state=abc&challenge=" + ch,
		"port too high": "port=70000&state=abc&challenge=" + ch,
		"empty state":   "port=5000&state=&challenge=" + ch,
		"bad state":     "port=5000&state=a;b&challenge=" + ch,
		"short chall":   "port=5000&state=abc&challenge=abc",
	} {
		rec := httptest.NewRecorder()
		a.cliLogin(rec, httptest.NewRequest("GET", "/auth/cli?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	a.cliLogin(rec, httptest.NewRequest("GET", "/auth/cli?port=5000&state=abc&challenge="+ch, nil))
	c := cookiesOf(rec)
	if rec.Code != http.StatusFound || c["cli_port"] != "5000" || c["cli_state"] != "abc" || c["cli_challenge"] != ch || c["oidc_state"] == "" {
		t.Fatalf("status %d, cookies %v", rec.Code, c)
	}
	if !strings.HasPrefix(rec.Header().Get("Location"), "https://idp.example/authorize") {
		t.Fatalf("redirect to %q", rec.Header().Get("Location"))
	}
}

func TestBrowserLoginClearsPendingCLILogin(t *testing.T) {
	a := newCLITestApp(t)
	rec := httptest.NewRecorder()
	a.login(rec, httptest.NewRequest("GET", "/auth/login", nil))
	for _, c := range rec.Result().Cookies() {
		if strings.HasPrefix(c.Name, "cli_") && c.MaxAge >= 0 {
			t.Fatalf("cookie %s not cleared", c.Name)
		}
	}
}

func cliCallback(a *App, user internals.User, port, state, challenge string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/auth/callback", nil)
	req.AddCookie(&http.Cookie{Name: "cli_port", Value: port})
	req.AddCookie(&http.Cookie{Name: "cli_state", Value: state})
	req.AddCookie(&http.Cookie{Name: "cli_challenge", Value: challenge})
	rec := httptest.NewRecorder()
	if !a.finishCLILogin(rec, req, user) {
		return nil
	}
	return rec
}

func exchange(a *App, code, verifier string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	rec := httptest.NewRecorder()
	a.cliToken(rec, httptest.NewRequest("POST", "/auth/cli/token", strings.NewReader(string(body))))
	return rec
}

func TestCLILoginEndToEnd(t *testing.T) {
	a := newCLITestApp(t)
	user := internals.User{Sub: "s", Iss: "i", Name: "Ada", Groups: []string{internals.AdminGroup}}
	verifier := "a-long-random-verifier"

	rec := cliCallback(a, user, "5000", "st4te", challengeFor(verifier))
	if rec == nil {
		t.Fatal("finishCLILogin declined a CLI login")
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Host != "127.0.0.1:5000" || loc.Path != "/callback" || loc.Query().Get("state") != "st4te" {
		t.Fatalf("redirect = %q, %v", rec.Header().Get("Location"), err)
	}
	if _, ok := cookiesOf(rec)["session"]; ok {
		t.Fatal("a CLI login must not set the browser session cookie")
	}
	code := loc.Query().Get("code")

	// wrong verifier burns the code
	if rec := exchange(a, code, "wrong"); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong verifier: %d", rec.Code)
	}
	if rec := exchange(a, code, verifier); rec.Code != http.StatusBadRequest {
		t.Fatalf("code reuse after failure: %d", rec.Code)
	}

	// a fresh login with the right verifier works exactly once
	loc2, _ := url.Parse(cliCallback(a, user, "5000", "st4te", challengeFor(verifier)).Header().Get("Location"))
	rec = exchange(a, loc2.Query().Get("code"), verifier)
	var out struct{ Token string }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Token == "" {
		t.Fatalf("exchange: %d %s", rec.Code, rec.Body)
	}
	if rec := exchange(a, loc2.Query().Get("code"), verifier); rec.Code != http.StatusBadRequest {
		t.Fatalf("second use: %d", rec.Code)
	}

	// the token authenticates API calls as that user
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	rec = httptest.NewRecorder()
	a.require(a.me)(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Ada") {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}

	// logout with the token ends the session
	rec = httptest.NewRecorder()
	a.logout(rec, req)
	rec = httptest.NewRecorder()
	a.require(a.me)(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", rec.Code)
	}
}

func TestFinishCLILoginIgnoresBrowserLogins(t *testing.T) {
	a := newCLITestApp(t)
	rec := httptest.NewRecorder()
	if a.finishCLILogin(rec, httptest.NewRequest("GET", "/auth/callback", nil), internals.User{}) {
		t.Fatal("no CLI cookies: must fall through to the browser login")
	}
}

func TestCLICodeExpires(t *testing.T) {
	var c cliCodes
	c.put("old", cliCode{expires: time.Now().Add(-time.Second)})
	if _, ok := c.take("old"); ok {
		t.Fatal("expired code accepted")
	}
}

func TestBearerAuth(t *testing.T) {
	a := newCLITestApp(t)
	for name, h := range map[string]string{"none": "", "no scheme": "abc", "empty": "Bearer ", "unknown": "Bearer nope"} {
		req := httptest.NewRequest("GET", "/api/me", nil)
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		rec := httptest.NewRecorder()
		a.require(a.me)(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}
