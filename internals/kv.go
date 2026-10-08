package internals

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	gonanoid "github.com/matoous/go-nanoid/v2"
	"go.etcd.io/bbolt"
)

const BoltDBPath string = "kv.db"
const MaxContentPieces int = 5

type Store struct {
	db *bbolt.DB
}

var sessionsBucket = []byte("sessions")
var submissionsTrackerBucket = []byte("submissions_tracker")
var submissionsBucket = []byte("submissions")
var userSubmissionsBucket = []byte("user_to_submissions")

func NewStore() (*Store, error) {
	return NewStoreAt(BoltDBPath)
}

func NewStoreAt(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 1 * time.Minute})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{sessionsBucket, submissionsBucket, submissionsTrackerBucket, userSubmissionsBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func key(id string) []byte {
	h := sha256.Sum256([]byte(id))
	return []byte(hex.EncodeToString(h[:]))
}

// userKey identifies a user by (sub, iss). The NUL separator keeps
// different (sub, iss) pairs from concatenating to the same string.
func userKey(user User) []byte {
	return key(user.Sub + "\x00" + user.Iss)
}

// getJSON returns (nil, nil) when the key is absent.
func getJSON[T any](b *bbolt.Bucket, k []byte) (*T, error) {
	data := b.Get(k)
	if data == nil {
		return nil, nil
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("error while unmarshaling: %w", err)
	}
	return &v, nil
}

func putJSON(b *bbolt.Bucket, k []byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("error while marshaling: %w", err)
	}
	return b.Put(k, data)
}

func (s *Store) PutSession(sessionId string, user User, ttl time.Duration) error {
	session := Session{User: user, ExpiresAt: time.Now().Add(ttl)}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket(sessionsBucket), key(sessionId), session)
	})
}

func (s *Store) GetSession(sessionId string) (*Session, error) {
	var session *Session
	err := s.db.View(func(tx *bbolt.Tx) error {
		var err error
		session, err = getJSON[Session](tx.Bucket(sessionsBucket), key(sessionId))
		return err
	})
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, errors.New("could not find session ID in the bucket")
	}
	if time.Now().After(session.ExpiresAt) {
		return nil, errors.New("entry has expired")
	}
	return session, nil
}

func (s *Store) DeleteSession(sessionId string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(sessionsBucket).Delete(key(sessionId))
	})
}

func (s *Store) CleanupSessions(every time.Duration) {
	for range time.Tick(every) {
		_ = s.db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket(sessionsBucket)
			// Deleting inside ForEach can skip keys, so collect first.
			var expired [][]byte
			_ = b.ForEach(func(k, v []byte) error {
				var session Session
				if err := json.Unmarshal(v, &session); err != nil || time.Now().After(session.ExpiresAt) {
					expired = append(expired, slices.Clone(k))
				}
				return nil
			})
			for _, k := range expired {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

// PutSubmission validates and stores a submission. The monthly limit check and
// all writes happen in a single transaction, so concurrent calls cannot exceed it.
func (s *Store) PutSubmission(user User, submission Submission) (string, error) {
	if err := submission.Validate(); err != nil {
		return "", err
	}
	id, err := gonanoid.New()
	if err != nil {
		return "", fmt.Errorf("error while creating ID for submission: %w", err)
	}
	if submission.SubmissionType == EventSubmissionType {
		id = "ev-" + id
	} else {
		id = "cont-" + id
	}

	now := time.Now().UTC()
	uid := userKey(user)
	submission.ID = id
	submission.Owner = string(uid)
	// Server-set from the session; anything the client sent is overwritten.
	submission.OwnerUsername = user.Login
	if submission.OwnerUsername == "" {
		submission.OwnerUsername = user.Name
	}
	submission.OwnerEmail = user.Email
	submission.Timestamp = now

	err = s.db.Update(func(tx *bbolt.Tx) error {
		trackers := tx.Bucket(submissionsTrackerBucket)
		tracker, err := getJSON[SubmissionTracker](trackers, uid)
		if err != nil {
			return err
		}
		if tracker == nil {
			tracker = &SubmissionTracker{}
		}
		if tracker.Year != now.Year() || tracker.Month != now.Month() {
			*tracker = SubmissionTracker{Year: now.Year(), Month: now.Month()}
		}
		if submission.SubmissionType == ContentSubmissionType {
			if tracker.ContentPieces >= MaxContentPieces {
				return ErrLimitReached
			}
			tracker.ContentPieces++
		} else {
			tracker.Events++
		}

		userSubs := tx.Bucket(userSubmissionsBucket)
		list, err := getJSON[UserSubmissions](userSubs, uid)
		if err != nil {
			return err
		}
		if list == nil {
			list = &UserSubmissions{Submissions: []string{}}
		}
		list.Submissions = append(list.Submissions, id)

		if err := putJSON(trackers, uid, tracker); err != nil {
			return err
		}
		if err := putJSON(tx.Bucket(submissionsBucket), []byte(id), submission); err != nil {
			return err
		}
		return putJSON(userSubs, uid, list)
	})
	if err != nil {
		if errors.Is(err, ErrLimitReached) {
			return "", err
		}
		return "", fmt.Errorf("error while storing submission: %w", err)
	}
	return id, nil
}

func (s *Store) GetSubmissionsByUser(user User) ([]Submission, error) {
	submissions := []Submission{}
	err := s.db.View(func(tx *bbolt.Tx) error {
		list, err := getJSON[UserSubmissions](tx.Bucket(userSubmissionsBucket), userKey(user))
		if err != nil || list == nil {
			return err
		}
		b := tx.Bucket(submissionsBucket)
		for _, id := range list.Submissions {
			sub, err := getJSON[Submission](b, []byte(id))
			if err != nil {
				return err
			}
			if sub == nil {
				continue // deleted out from under the list
			}
			submissions = append(submissions, *sub)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error while getting submissions for the user: %w", err)
	}
	return submissions, nil
}

func (s *Store) GetSubmission(submissionId string, user User) (*Submission, error) {
	var sub *Submission
	err := s.db.View(func(tx *bbolt.Tx) error {
		var err error
		sub, err = getJSON[Submission](tx.Bucket(submissionsBucket), []byte(submissionId))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("error while getting submission %s: %w", submissionId, err)
	}
	if sub == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, submissionId)
	}
	if !user.IsAdmin() && sub.Owner != string(userKey(user)) {
		return nil, fmt.Errorf("%w: %s", ErrForbidden, submissionId)
	}
	return sub, nil
}

// Note: deleting a submission does not decrement the submissions tracker count
func (s *Store) DeleteSubmission(submissionId string, user User) error {
	err := s.db.Update(func(tx *bbolt.Tx) error {
		subs := tx.Bucket(submissionsBucket)
		sub, err := getJSON[Submission](subs, []byte(submissionId))
		if err != nil {
			return err
		}
		if sub == nil {
			return fmt.Errorf("%w: %s", ErrNotFound, submissionId)
		}
		if !user.IsAdmin() && sub.Owner != string(userKey(user)) {
			return fmt.Errorf("%w: %s", ErrForbidden, submissionId)
		}
		if err := subs.Delete([]byte(submissionId)); err != nil {
			return err
		}
		userSubs := tx.Bucket(userSubmissionsBucket)
		list, err := getJSON[UserSubmissions](userSubs, []byte(sub.Owner))
		if err != nil || list == nil {
			return err
		}
		list.Submissions = slices.DeleteFunc(list.Submissions, func(id string) bool { return id == submissionId })
		return putJSON(userSubs, []byte(sub.Owner), list)
	})
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
		return err
	}
	if err != nil {
		return fmt.Errorf("error while deleting submission %s: %w", submissionId, err)
	}
	return nil
}

func (s *Store) GetSubmissions(u User) ([]Submission, error) {
	if !u.IsAdmin() {
		return nil, ErrForbidden
	}
	submissions := []Submission{}
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(submissionsBucket)
		return b.ForEach(func(k, v []byte) error {
			var s Submission
			err := json.Unmarshal(v, &s)
			if err != nil {
				return err
			}
			submissions = append(submissions, s)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return submissions, nil
}

// GetContentQuota returns how many content pieces the user can still submit this month.
func (s *Store) GetContentQuota(u User) (int, error) {
	var tracker *SubmissionTracker
	err := s.db.View(func(tx *bbolt.Tx) error {
		var err error
		tracker, err = getJSON[SubmissionTracker](tx.Bucket(submissionsTrackerBucket), userKey(u))
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("error while getting quota: %w", err)
	}
	now := time.Now().UTC()
	// No tracker, or one from a previous month: the full quota is available.
	if tracker == nil || tracker.Year != now.Year() || tracker.Month != now.Month() {
		return MaxContentPieces, nil
	}
	return max(MaxContentPieces-tracker.ContentPieces, 0), nil
}
