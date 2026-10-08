package internals

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastRetries(t *testing.T) {
	old := BaseSleepTime
	BaseSleepTime = time.Millisecond
	t.Cleanup(func() { BaseSleepTime = old })
}

func TestSlackRetriesWithFullBody(t *testing.T) {
	fastRetries(t)
	var calls atomic.Int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("SLACK_WEBHOOK_URL", srv.URL)
	c, err := NewSlackClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SendNotification(context.Background(), "desc", "alice", "a@example.com", nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
	for i, b := range bodies {
		if !strings.Contains(b, "New Submission") {
			t.Errorf("attempt %d sent an empty/partial body: %q", i+1, b)
		}
	}
}

func TestSlackGivesUpAndDoesNotRetryClientErrors(t *testing.T) {
	fastRetries(t)
	for _, tc := range []struct{ status, wantCalls int }{{500, MaxAttempts}, {400, 1}} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(tc.status)
		}))
		t.Setenv("SLACK_WEBHOOK_URL", srv.URL)
		c, _ := NewSlackClient()
		if err := c.SendNotification(context.Background(), "d", "n", "e", nil, nil); err == nil {
			t.Errorf("status %d: expected an error", tc.status)
		}
		if int(calls.Load()) != tc.wantCalls {
			t.Errorf("status %d: calls = %d, want %d", tc.status, calls.Load(), tc.wantCalls)
		}
		srv.Close()
	}
}

func TestSlackMessageIsPlainText(t *testing.T) {
	url := "https://example.com/a?x=1&y=2"
	msg := buildMessage("a blog about <things> & stuff", "alice", "a@example.com", &url,
		&LlmEvaluationOutput{Review: "Solid post.", Rating: 7})
	// No Slack mrkdwn markup and no HTML entities: the workflow shows text verbatim.
	for _, bad := range []string{"*", "`", "<", ">", "&amp;", "&lt;", "mailto:"} {
		if strings.Contains(strings.ReplaceAll(msg, "<things>", ""), bad) {
			t.Errorf("message contains %q:\n%s", bad, msg)
		}
	}
	for _, want := range []string{"New Submission!", "alice (a@example.com)", url, "a blog about <things> & stuff", "AI Review:\nSolid post.", "Rated: 7/10"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q:\n%s", want, msg)
		}
	}
	none := buildMessage("d", "n", "", nil, nil)
	if !strings.Contains(none, "URL:\nNONE") || !strings.Contains(none, "Rated: NA") || strings.Contains(none, "()") {
		t.Errorf("unexpected message:\n%s", none)
	}
}

func TestSlackSendHonorsContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release) // runs before srv.Close, so the handler can return
	t.Setenv("SLACK_WEBHOOK_URL", srv.URL)
	c, _ := NewSlackClient()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.SendNotification(ctx, "d", "n", "e", nil, nil); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}
