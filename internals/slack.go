package internals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const MaxAttempts int = 3

var BaseSleepTime = time.Second

const maxSlackFieldRunes = 2500

type SlackClient struct {
	webhook string
	client  *http.Client
}

func NewSlackClient() (*SlackClient, error) {
	hook, ok := os.LookupEnv("SLACK_WEBHOOK_URL")
	if !ok || hook == "" {
		return nil, errors.New("could not find SLACK_WEBHOOK_URL in the current environment")
	}
	return &SlackClient{webhook: hook, client: &http.Client{Timeout: 15 * time.Second}}, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// field truncates untrusted text for inclusion in a message.
func field(s string) string { return truncate(s, maxSlackFieldRunes) }

// buildMessage returns plain text on purpose: the webhook is a Slack Workflow
// whose "message" variable is inserted verbatim, so mrkdwn (*bold*, `code`,
// <url|label>) would show up as literal characters.
func buildMessage(description, ownerName, ownerEmail string, url *string, evaluation *LlmEvaluationOutput) string {
	var b strings.Builder
	b.WriteString("New Submission!\n\nFrom:\n")
	name := field(ownerName)
	if name == "" {
		name = "unknown"
	}
	b.WriteString(name)
	if ownerEmail != "" {
		fmt.Fprintf(&b, " (%s)", field(ownerEmail))
	}
	b.WriteString("\n\nURL:\n")
	if url != nil && *url != "" {
		b.WriteString(field(*url))
	} else {
		b.WriteString("NONE")
	}
	fmt.Fprintf(&b, "\n\nDescription:\n%s\n\nAI Review:\n", field(description))
	if evaluation != nil {
		fmt.Fprintf(&b, "%s\n\nRated: %f/10", field(evaluation.Review), evaluation.Rating)
	} else {
		b.WriteString("NA\n\nRated: NA")
	}
	return b.String()
}

func (s *SlackClient) SendNotification(ctx context.Context, submissionDescription, submissionOwnerName, submissionOwnerEmail string, submissionUrl *string, evaluation *LlmEvaluationOutput) error {
	msg := buildMessage(submissionDescription, submissionOwnerName, submissionOwnerEmail, submissionUrl, evaluation)
	data, err := json.Marshal(map[string]string{"message": msg})
	if err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		retry, err := s.post(ctx, data)
		if err == nil {
			return nil
		}
		if !retry {
			return err
		}
		if attempt >= MaxAttempts {
			return fmt.Errorf("maximum number of attempts exceeded, could not send notification to Slack: %w", err)
		}
		select {
		case <-time.After(time.Duration(attempt) * BaseSleepTime):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// post makes one attempt. The request is rebuilt each time, since a request body
// can only be read once. The bool reports whether trying again could help.
func (s *SlackClient) post(ctx context.Context, data []byte) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhook, bytes.NewReader(data))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return ctx.Err() == nil, fmt.Errorf("error while sending the notification to Slack: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	err = fmt.Errorf("slack responded with status code %d: %s", resp.StatusCode, body)
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500, err
}
