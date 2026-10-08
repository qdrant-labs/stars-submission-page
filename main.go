package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/qdrant-labs/stars-submission-page/internals"
	"github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	"golang.org/x/oauth2"
)

//go:embed web
var webFS embed.FS

type App struct {
	oauth              *oauth2.Config
	verifier           *oidc.IDTokenVerifier
	store              *internals.Store // swap for a DB/Redis later
	secure             bool
	llm                *internals.LLM
	submissionsChannel chan<- internals.Submission
	rateLimiter        *limiter.Limiter
}

func randString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *App) setCookie(w http.ResponseWriter, name, val, path string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: val, Path: path,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	state := randString(24)
	nonce := randString(24)
	verifier := oauth2.GenerateVerifier()
	a.setCookie(w, "oidc_state", state, "/auth", 10*time.Minute)
	a.setCookie(w, "oidc_verifier", verifier, "/auth", 10*time.Minute)
	a.setCookie(w, "oidc_nonce", nonce, "/auth", 10*time.Minute)
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusFound)
}

func (a *App) callback(w http.ResponseWriter, r *http.Request) {
	sc, err1 := r.Cookie("oidc_state")
	vc, err2 := r.Cookie("oidc_verifier")
	nc, err3 := r.Cookie("oidc_nonce")
	if err1 != nil || err2 != nil || err3 != nil ||
		subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(sc.Value)) != 1 {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	tok, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(vc.Value))
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token", http.StatusBadGateway)
		return
	}
	idt, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		http.Error(w, "invalid id_token", http.StatusUnauthorized)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nc.Value)) != 1 {
		http.Error(w, "invalid nonce", http.StatusBadRequest)
		return
	}
	var u internals.User
	if err := idt.Claims(&u); err != nil {
		http.Error(w, "bad claims", http.StatusInternalServerError)
		return
	}

	sid := randString(32)
	if err := a.store.PutSession(sid, u, 24*time.Hour); err != nil {
		log.Printf("could not store session: %v", err)
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	a.setCookie(w, "session", sid, "/", 24*time.Hour)
	a.setCookie(w, "oidc_state", "", "/auth", -1)
	a.setCookie(w, "oidc_verifier", "", "/auth", -1)
	a.setCookie(w, "oidc_nonce", "", "/auth", -1)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *App) userFrom(r *http.Request) (internals.User, bool) {
	c, err := r.Cookie("session")
	if err != nil {
		return internals.User{}, false
	}
	s, err := a.store.GetSession(c.Value)
	if err != nil {
		return internals.User{}, false
	}
	return s.User, true
}

// Middleware for protected API routes
func (a *App) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.userFrom(r); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a *App) me(w http.ResponseWriter, r *http.Request) {
	u, _ := a.userFrom(r)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(u)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		a.store.DeleteSession(c.Value)
	}
	a.setCookie(w, "session", "", "/", -1)
	w.WriteHeader(http.StatusNoContent)
}

// writeStoreError maps store errors to HTTP statuses.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, internals.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, internals.ErrLimitReached):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, internals.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, internals.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		log.Printf("store error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("could not encode response: %v", err)
	}
}

const maxBodyBytes = 64 << 10

func (a *App) submit(w http.ResponseWriter, r *http.Request) {
	var req internals.Submission
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}
	u, _ := a.userFrom(r)
	submissionId, err := a.store.PutSubmission(u, req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	a.notify(submissionId, u)
	writeJSON(w, internals.SubmissionResponse{ID: submissionId})
}

// notify queues the stored submission (validated, with the owner's identity filled
// in) for evaluation and the Slack notification. It never blocks the request.
func (a *App) notify(submissionId string, u internals.User) {
	stored, err := a.store.GetSubmission(submissionId, u)
	if err != nil {
		log.Printf("could not load submission %s for notification: %v", submissionId, err)
		return
	}
	select {
	case a.submissionsChannel <- *stored:
	default:
		log.Printf("notification queue is full, dropping notification for submission %s", submissionId)
	}
}

func (a *App) getSubmissionsByUser(w http.ResponseWriter, r *http.Request) {
	u, _ := a.userFrom(r)
	submissions, err := a.store.GetSubmissionsByUser(u)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, internals.GetSubmissionsResponse{Submissions: submissions})
}

func (a *App) getSubmission(w http.ResponseWriter, r *http.Request) {
	u, _ := a.userFrom(r)
	submission, err := a.store.GetSubmission(r.PathValue("id"), u)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, submission)
}

func (a *App) deleteSubmission(w http.ResponseWriter, r *http.Request) {
	u, _ := a.userFrom(r)
	if err := a.store.DeleteSubmission(r.PathValue("id"), u); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) getQuota(w http.ResponseWriter, r *http.Request) {
	u, _ := a.userFrom(r)
	quota, err := a.store.GetContentQuota(u)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, internals.GetQuotaResponse{Quota: quota})
}

func (a *App) getSubmissions(w http.ResponseWriter, r *http.Request) {
	u, _ := a.userFrom(r)
	submissions, err := a.store.GetSubmissions(u)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, internals.GetSubmissionsResponse{Submissions: submissions})
}

func (a *App) getSuggestions(w http.ResponseWriter, r *http.Request) {
	var req internals.GetSuggestionsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}
	// Validate before spending one of the user's daily reviews (and LLM money).
	check := internals.Submission{SubmissionType: internals.ContentSubmissionType, Url: &req.Url, Details: req.Description}
	if req.Url == "" {
		writeStoreError(w, fmt.Errorf("%w: url is required", internals.ErrInvalid))
		return
	}
	if err := check.Validate(); err != nil {
		writeStoreError(w, err)
		return
	}

	u, _ := a.userFrom(r)
	ctx, err := a.rateLimiter.Get(r.Context(), string(internals.UserKey(u)))
	if err != nil {
		log.Printf("rate limiter error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(ctx.Reset, 10))
	w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(ctx.Limit, 10))
	w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(ctx.Remaining, 10))
	if ctx.Reached {
		http.Error(w, "You already used your 2 AI reviews for today", http.StatusTooManyRequests)
		return
	}
	llmCtx, cancel := context.WithTimeout(r.Context(), llmTimeout)
	defer cancel()
	review, err := a.llm.GetSuggestions(llmCtx, *check.Url, check.Details)
	if err != nil {
		log.Printf("could not generate suggestions: %v", err)
		http.Error(w, "could not generate suggestions right now, please try again later", http.StatusBadGateway)
		return
	}
	writeJSON(w, review)
}

const llmTimeout = 3 * time.Minute

// evalWorker evaluates content submissions with the LLM and posts the result to
// Slack. If the evaluation fails, the notification is still sent without a review.
func evalWorker(ctx context.Context, subs <-chan internals.Submission, llm *internals.LLM, slack *internals.SlackClient) {
	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping evaluation worker")
			return
		case s := <-subs:
			var evaluation *internals.LlmEvaluationOutput
			if s.Url != nil && s.SubmissionType == internals.ContentSubmissionType {
				evalCtx, cancel := context.WithTimeout(ctx, llmTimeout)
				ev, err := llm.GetEvaluation(evalCtx, *s.Url, s.Details)
				cancel()
				if err != nil {
					log.Printf("could not evaluate submission %s: %v", s.ID, err)
				} else {
					evaluation = ev
				}
			}
			if err := slack.SendNotification(ctx, s.Details, s.OwnerUsername, s.OwnerEmail, s.Url, evaluation); err != nil {
				log.Printf("could not send the Slack notification for submission %s: %v", s.ID, err)
			}
		}
	}
}

func main() {
	// Cancelled on Ctrl-C or SIGTERM; every worker and the server shut down from this.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	issuer := os.Getenv("OIDC_ISSUER")                       // = Pocket ID APP_URL
	baseURL := strings.TrimRight(os.Getenv("BASE_URL"), "/") // e.g. http://localhost:8080
	clientID := os.Getenv("OIDC_CLIENT_ID")
	secret := os.Getenv("OIDC_CLIENT_SECRET")
	for name, v := range map[string]string{"OIDC_ISSUER": issuer, "BASE_URL": baseURL, "OIDC_CLIENT_ID": clientID, "OIDC_CLIENT_SECRET": secret} {
		if v == "" {
			log.Fatalf("missing required environment variable %s", name)
		}
	}
	store, err := internals.NewStore()
	if err != nil {
		log.Fatal(err)
	}
	llm, err := internals.NewLLM()
	if err != nil {
		log.Fatal(err)
	}
	slack, err := internals.NewSlackClient()
	if err != nil {
		log.Fatal(err)
	}
	// 2 AI reviews per user per day (in-memory: resets when the server restarts)
	rate, err := limiter.NewRateFromFormatted("2-D")
	if err != nil {
		log.Fatal(err)
	}
	rateLimiter := limiter.New(memory.NewStore(), rate)

	subChan := make(chan internals.Submission, 64)
	var wg sync.WaitGroup
	wg.Go(func() { store.CleanupSessions(ctx, time.Hour) })
	wg.Go(func() { evalWorker(ctx, subChan, llm, slack) })

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		log.Fatal(err)
	}
	app := &App{
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: secret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  baseURL + "/auth/callback",
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "groups"},
		},
		verifier:           provider.Verifier(&oidc.Config{ClientID: clientID}),
		store:              store,
		secure:             strings.HasPrefix(baseURL, "https://"),
		submissionsChannel: subChan,
		llm:                llm,
		rateLimiter:        rateLimiter,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", app.login)
	mux.HandleFunc("GET /auth/callback", app.callback)
	mux.HandleFunc("POST /auth/logout", app.logout)
	mux.HandleFunc("GET /api/me", app.require(app.me))
	mux.HandleFunc("GET /api/quota", app.require(app.getQuota))
	mux.HandleFunc("POST /api/submissions", app.require(app.submit))
	mux.HandleFunc("GET /api/submissions", app.require(app.getSubmissionsByUser))
	mux.HandleFunc("GET /api/submissions/{id}", app.require(app.getSubmission))
	mux.HandleFunc("DELETE /api/submissions/{id}", app.require(app.deleteSubmission))
	mux.HandleFunc("GET /api/admin/submissions", app.require(app.getSubmissions))
	mux.HandleFunc("POST /api/suggestions", app.require(app.getSuggestions))

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	httpServer := &http.Server{
		Addr:              ":" + getenv("PORT", "8080"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Println("listening on", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Server error: %s", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Println("Shutting down server and workers...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server shutdown error: %s", err)
	}
	wg.Wait()
	log.Println("Application stopped")
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
