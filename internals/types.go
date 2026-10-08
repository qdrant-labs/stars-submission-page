package internals

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

type SubmissionType = string

const ContentSubmissionType SubmissionType = "content"
const EventSubmissionType SubmissionType = "event"
const InvalidSubmissionType SubmissionType = ""
const AdminGroup string = "qdrant-admins"

func ValidateSubmissionType(submissionType string) (SubmissionType, error) {
	switch submissionType {
	case "content":
		return ContentSubmissionType, nil
	case "event":
		return EventSubmissionType, nil
	default:
		return InvalidSubmissionType, fmt.Errorf("Invalid submission type: %s", submissionType)
	}
}

type User struct {
	Sub    string   `json:"sub"`
	Iss    string   `json:"iss"`
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Login  string   `json:"preferred_username"`
	Groups []string `json:"groups"`
}

func (u User) IsAdmin() bool {
	return slices.Contains(u.Groups, AdminGroup)
}

type Session struct {
	User      User      `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
}

var (
	ErrLimitReached = fmt.Errorf("you already reached the maximum number of content pieces you can submit for review during this month: %d", MaxContentPieces)
	ErrForbidden    = errors.New("user does not have permissions to access this submission")
	ErrNotFound     = errors.New("submission does not exist")
	ErrInvalid      = errors.New("invalid submission")
)

const (
	MaxDetailsLen = 5000
	MaxURLLen     = 2048
)

type SubmissionTracker struct {
	Year          int        `json:"year"`
	Month         time.Month `json:"month"`
	ContentPieces int        `json:"content_pieces"`
	Events        int        `json:"events"`
}

type Submission struct {
	ID             string         `json:"id"`
	Owner          string         `json:"owner,omitempty"`
	OwnerUsername  string         `json:"owner_username,omitempty"`
	OwnerEmail     string         `json:"owner_email,omitempty"`
	SubmissionType SubmissionType `json:"type"`
	Url            *string        `json:"url,omitempty"`
	Details        string         `json:"description"`
	Timestamp      time.Time      `json:"timestamp"`
}

// Validate checks client-provided fields and normalizes them in place.
func (s *Submission) Validate() error {
	t, err := ValidateSubmissionType(s.SubmissionType)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err.Error())
	}
	s.SubmissionType = t
	s.Details = strings.TrimSpace(s.Details)
	if s.Details == "" {
		return fmt.Errorf("%w: description is required", ErrInvalid)
	}
	if len(s.Details) > MaxDetailsLen {
		return fmt.Errorf("%w: description is longer than %d bytes", ErrInvalid, MaxDetailsLen)
	}
	if s.Url != nil {
		raw := strings.TrimSpace(*s.Url)
		if raw == "" {
			s.Url = nil
		} else {
			u, err := url.Parse(raw)
			if err != nil || len(raw) > MaxURLLen || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("%w: url must be a valid http(s) URL", ErrInvalid)
			}
			s.Url = &raw
		}
	}
	return nil
}

type UserSubmissions struct {
	Submissions []string `json:"submissions"`
}

type SubmissionResponse struct {
	ID string `json:"id"`
}

type GetSubmissionsResponse struct {
	Submissions []Submission `json:"submissions"`
}

type GetQuotaResponse struct {
	Quota int `json:"quota"`
}

type GetSuggestionsRequest struct {
	Url         string `json:"url"`
	Description string `json:"description"`
}
