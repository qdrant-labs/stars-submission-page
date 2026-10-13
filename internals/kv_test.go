package internals

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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
	if q, err := s.GetQuotas(alice); err != nil || q.Content != MaxContentPieces {
		t.Fatalf("fresh: %+v, %v", q, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.PutSubmission(alice, content()); err != nil {
			t.Fatal(err)
		}
	}
	if q, _ := s.GetQuotas(alice); q.Content != MaxContentPieces-2 {
		t.Fatalf("after 2: %+v", q)
	}
	if q, _ := s.GetQuotas(bob); q.Content != MaxContentPieces {
		t.Fatalf("bob: %+v", q)
	}
	// a tracker from an earlier month must not reduce the quota
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket(submissionsTrackerBucket), UserKey(alice),
			SubmissionTracker{Year: time.Now().Year() - 1, Month: time.Now().Month(), ContentPieces: MaxContentPieces})
	})
	if err != nil {
		t.Fatal(err)
	}
	if q, _ := s.GetQuotas(alice); q.Content != MaxContentPieces {
		t.Fatalf("stale tracker: %+v", q)
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

func submissionOf(typ string) Submission { return Submission{SubmissionType: typ, Details: "thing"} }

func TestCodeSubmissions(t *testing.T) {
	s := newTestStore(t)
	id, err := s.PutSubmission(alice, submissionOf(CodeSubmissionType))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "code-") {
		t.Fatalf("id = %q, want code- prefix", id)
	}
	sub, err := s.GetSubmission(id, alice)
	if err != nil || sub.SubmissionType != CodeSubmissionType {
		t.Fatalf("got %+v, %v", sub, err)
	}
}

func TestCodeLimit(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < MaxCodePieces; i++ {
		if _, err := s.PutSubmission(alice, submissionOf(CodeSubmissionType)); err != nil {
			t.Fatalf("code %d: %v", i, err)
		}
	}
	if _, err := s.PutSubmission(alice, submissionOf(CodeSubmissionType)); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("over the code limit: %v", err)
	}
	// other users and other types are unaffected
	if _, err := s.PutSubmission(bob, submissionOf(CodeSubmissionType)); err != nil {
		t.Fatalf("bob: %v", err)
	}
	for _, typ := range []string{ContentSubmissionType, EventSubmissionType, OtherSubmissionType} {
		if _, err := s.PutSubmission(alice, submissionOf(typ)); err != nil {
			t.Fatalf("%s after code limit: %v", typ, err)
		}
	}
}

func TestLimitsAreIndependent(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < MaxContentPieces; i++ {
		if _, err := s.PutSubmission(alice, content()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutSubmission(alice, content()); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("content over limit: %v", err)
	}
	if _, err := s.PutSubmission(alice, submissionOf(CodeSubmissionType)); err != nil {
		t.Fatalf("code after content limit: %v", err)
	}
	q, err := s.GetQuotas(alice)
	if err != nil || q.Content != 0 || q.Code != MaxCodePieces-1 {
		t.Fatalf("quotas = %+v, %v", q, err)
	}
}

func TestOtherAndEventAreUnlimited(t *testing.T) {
	s := newTestStore(t)
	for _, typ := range []string{EventSubmissionType, OtherSubmissionType} {
		for i := 0; i < MaxContentPieces+MaxCodePieces+1; i++ {
			if _, err := s.PutSubmission(alice, submissionOf(typ)); err != nil {
				t.Fatalf("%s %d: %v", typ, i, err)
			}
		}
	}
	if q, _ := s.GetQuotas(alice); q.Content != MaxContentPieces || q.Code != MaxCodePieces {
		t.Fatalf("unlimited types must not use quota: %+v", q)
	}
}

func TestOtherSubmissionID(t *testing.T) {
	s := newTestStore(t)
	id, err := s.PutSubmission(alice, submissionOf(OtherSubmissionType))
	if err != nil || !strings.HasPrefix(id, "other-") {
		t.Fatalf("id = %q, %v", id, err)
	}
}

func TestCodeQuotaResetsNextMonth(t *testing.T) {
	s := newTestStore(t)
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket(submissionsTrackerBucket), UserKey(alice),
			SubmissionTracker{Year: time.Now().Year() - 1, Month: time.Now().Month(), CodePieces: MaxCodePieces})
	})
	if err != nil {
		t.Fatal(err)
	}
	if q, _ := s.GetQuotas(alice); q.Code != MaxCodePieces {
		t.Fatalf("stale tracker: %+v", q)
	}
	if _, err := s.PutSubmission(alice, submissionOf(CodeSubmissionType)); err != nil {
		t.Fatalf("new month should reset the code counter: %v", err)
	}
}

func TestQuotaResponseJSON(t *testing.T) {
	b, err := json.Marshal(GetQuotaResponse{Content: 3, Code: 7})
	if err != nil || string(b) != `{"content":3,"code":7}` {
		t.Fatalf("%s, %v", b, err)
	}
}

func TestValidateCodeType(t *testing.T) {
	for _, typ := range []string{"content", "code", "event", "other"} {
		if got, err := ValidateSubmissionType(typ); err != nil || got != typ {
			t.Fatalf("%s: %q, %v", typ, got, err)
		}
	}
	if _, err := ValidateSubmissionType("Code"); err == nil {
		t.Fatal("type names are case-sensitive")
	}
}
