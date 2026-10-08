package internals

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStoreAt(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

var alice = User{Sub: "a", Iss: "https://idp"}
var bob = User{Sub: "b", Iss: "https://idp"}
var admin = User{Sub: "root", Iss: "https://idp", Groups: []string{AdminGroup}}

func content() Submission { return Submission{SubmissionType: "content", Details: "post"} }

func TestSubmissionLifecycle(t *testing.T) {
	s := newTestStore(t)
	id, err := s.PutSubmission(alice, content())
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.GetSubmissionsByUser(alice)
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Timestamp.IsZero() {
		t.Fatalf("list = %+v, err = %v", list, err)
	}
	if _, err := s.GetSubmission(id, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSubmission(id, bob); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob get: %v", err)
	}
	if _, err := s.GetSubmission(id, admin); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSubmission(id, bob); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob delete: %v", err)
	}
	if err := s.DeleteSubmission(id, admin); err != nil {
		t.Fatal(err)
	}
	if list, err := s.GetSubmissionsByUser(alice); err != nil || len(list) != 0 {
		t.Fatalf("after delete: %+v, %v", list, err)
	}
	if err := s.DeleteSubmission(id, alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestMonthlyLimitConcurrent(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, limited := 0, 0
	for range 20 {
		wg.Go(func() {
			_, err := s.PutSubmission(alice, content())
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if errors.Is(err, ErrLimitReached) {
				limited++
			} else {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if ok != MaxContentPieces || limited != 20-MaxContentPieces {
		t.Fatalf("ok=%d limited=%d", ok, limited)
	}
	// events are not capped, and deleting does not free a slot
	if _, err := s.PutSubmission(alice, Submission{SubmissionType: "event", Details: "talk"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidation(t *testing.T) {
	s := newTestStore(t)
	bad := "ftp://x"
	for _, sub := range []Submission{
		{SubmissionType: "bogus", Details: "x"},
		{SubmissionType: "content", Details: "  "},
		{SubmissionType: "content", Details: "x", Url: &bad},
	} {
		if _, err := s.PutSubmission(alice, sub); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", sub, err)
		}
	}
}

func TestSessions(t *testing.T) {
	s := newTestStore(t)
	if err := s.PutSession("sid", alice, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetSession("sid"); err != nil || got.User.Sub != "a" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.GetSession("nope"); err == nil {
		t.Fatal("expected error")
	}
	_ = s.PutSession("old", alice, -time.Minute)
	if _, err := s.GetSession("old"); err == nil {
		t.Fatal("expected expiry")
	}
}

func TestContentQuota(t *testing.T) {
	s := newTestStore(t)
	if q, err := s.GetContentQuota(alice); err != nil || q != MaxContentPieces {
		t.Fatalf("fresh: %d, %v", q, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.PutSubmission(alice, content()); err != nil {
			t.Fatal(err)
		}
	}
	if q, _ := s.GetContentQuota(alice); q != MaxContentPieces-2 {
		t.Fatalf("after 2: %d", q)
	}
	if q, _ := s.GetContentQuota(bob); q != MaxContentPieces {
		t.Fatalf("bob: %d", q)
	}
	// a tracker from an earlier month must not reduce the quota
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket(submissionsTrackerBucket), UserKey(alice),
			SubmissionTracker{Year: time.Now().Year() - 1, Month: time.Now().Month(), ContentPieces: MaxContentPieces})
	})
	if err != nil {
		t.Fatal(err)
	}
	if q, _ := s.GetContentQuota(alice); q != MaxContentPieces {
		t.Fatalf("stale tracker: %d", q)
	}
}

func TestOwnerIdentityIsServerSet(t *testing.T) {
	s := newTestStore(t)
	u := User{Sub: "u", Iss: "i", Login: "alice", Email: "alice@example.com"}
	sub := content()
	sub.OwnerEmail, sub.OwnerUsername = "evil@example.com", "evil"
	id, err := s.PutSubmission(u, sub)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSubmission(id, u)
	if err != nil || got.OwnerUsername != "alice" || got.OwnerEmail != "alice@example.com" {
		t.Fatalf("%+v %v", got, err)
	}
}
