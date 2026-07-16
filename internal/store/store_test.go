package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDurableDuplicateAcrossReopenAndRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := "019f64ca-3000-7000-8000-000000000105"
	intent := ActionIntent{ActionID: id, OperationType: "agent.read", Issuer: "issuer", Subject: "subject", HostID: "host", InstanceID: "default", TerminalID: "term"}
	if err = s.BeginAction(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(ctx, id, "failed", str("CONNECTION_LOST"), time.Now()); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(100 * 24 * time.Hour) }
	if err = s.Retain(ctx, 90*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BeginAction(ctx, intent); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate was not durable: %v", err)
	}
}

func TestEnrollmentExpiryAndReuse(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	if err := s.CreateEnrollment(ctx, HashToken("expired"), "host", "name", now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollmentForToken(ctx, HashToken("expired")); !errors.Is(err, ErrEnrollmentUsed) {
		t.Fatal("expired token accepted")
	}
	if err := s.CreateEnrollment(ctx, HashToken("once"), "host", "name", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cert := Certificate{Serial: "1", HostID: "host", Fingerprint: "fingerprint", NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	if err := s.CommitEnrollmentCertificate(ctx, HashToken("once"), cert); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitEnrollmentCertificate(ctx, HashToken("once"), Certificate{Serial: "2", HostID: "host", Fingerprint: "other", NotBefore: now, NotAfter: now.Add(time.Hour)}); !errors.Is(err, ErrEnrollmentUsed) {
		t.Fatal("reused token accepted")
	}
}

func TestAuditSchemaCannotPersistTranscript(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.db.Query(`PRAGMA table_info(actions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "text", "keys", "prompt", "output", "content", "csr", "token":
			t.Fatalf("sensitive audit column %q", name)
		}
	}
}
func TestStartupRecoveryFinalizesIncompleteActions(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, v := range []struct{ id, op string }{{"019f64ca-3000-7000-8000-000000000111", "agent.read"}, {"019f64ca-3000-7000-8000-000000000112", "agent.send_text"}} {
		if err := s.BeginAction(ctx, ActionIntent{ActionID: v.id, OperationType: v.op, Issuer: "i", Subject: "s", HostID: "h", InstanceID: "default", TerminalID: "term"}); err != nil {
			t.Fatal(err)
		}
	}
	count, err := s.RecoverIncomplete(ctx)
	if err != nil || count != 2 {
		t.Fatalf("recovery = %d, %v", count, err)
	}
	read, _ := s.Action(ctx, "019f64ca-3000-7000-8000-000000000111")
	write, _ := s.Action(ctx, "019f64ca-3000-7000-8000-000000000112")
	if read.Status != "failed" || *read.Code != "CONNECTION_LOST" {
		t.Fatalf("read recovery = %#v", read)
	}
	if write.Status != "unknown" || *write.Code != "OUTCOME_UNKNOWN" {
		t.Fatalf("write recovery = %#v", write)
	}
	var audits int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE kind='action.recovered'`).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("recovery audits = %d, %v", audits, err)
	}
}
func TestMigrationsAreOrderedAndRecorded(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if len(versions) != 3 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 {
		t.Fatalf("migration versions = %v", versions)
	}
}
func TestPushSubscriptionBoundsAndPerOperatorLimit(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < MaxPushSubscriptionsPerOperator; i++ {
		endpoint := fmt.Sprintf("https://push.example/%d", i)
		if err := s.UpsertPush(ctx, PushSubscription{Subject: "operator", Endpoint: endpoint, P256DH: "key", Auth: "auth", UserAgent: "browser"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertPush(ctx, PushSubscription{Subject: "operator", Endpoint: "https://push.example/overflow", P256DH: "key", Auth: "auth"}); !errors.Is(err, ErrPushLimit) {
		t.Fatalf("overflow push = %v", err)
	}
	invalid := []PushSubscription{{Subject: "other", Endpoint: "https://push.example/other", P256DH: strings.Repeat("k", 513), Auth: "auth"}, {Subject: "other", Endpoint: "https://push.example/other", P256DH: "key", Auth: strings.Repeat("a", 257)}, {Subject: "other", Endpoint: "https://push.example/" + strings.Repeat("e", 2049), P256DH: "key", Auth: "auth"}, {Subject: "other", Endpoint: "https://push.example/other", P256DH: "key", Auth: "auth", UserAgent: strings.Repeat("u", 257)}}
	for _, endpoint := range []string{"https://127.0.0.1/push", "https://user@push.example/push", "https://push.example:8443/push", "https://push.example/push#fragment"} {
		invalid = append(invalid, PushSubscription{Subject: "other", Endpoint: endpoint, P256DH: "key", Auth: "auth"})
	}
	for _, subscription := range invalid {
		if err := s.UpsertPush(ctx, subscription); !errors.Is(err, ErrInvalidPushSubscription) {
			t.Fatalf("invalid subscription accepted: %#v, %v", subscription, err)
		}
	}
	subscriptions, err := s.PushSubscriptions(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != MaxPushSubscriptionsPerOperator {
		t.Fatalf("subscriptions = %d", len(subscriptions))
	}
	if subscriptions[0].UserAgent != "browser" {
		t.Fatalf("user agent not retained: %#v", subscriptions[0])
	}
}
func TestPushCleanupRemovesStaleSubscriptions(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	if err := s.UpsertPush(ctx, PushSubscription{Subject: "operator", Endpoint: "https://push.example/stale", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(91 * 24 * time.Hour) }
	if err := s.UpsertPush(ctx, PushSubscription{Subject: "operator", Endpoint: "https://push.example/current", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	subscriptions, err := s.PushSubscriptions(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != 1 || subscriptions[0].Endpoint != "https://push.example/current" {
		t.Fatalf("stale cleanup = %#v", subscriptions)
	}
}
func TestPushReplacementIsAtomicAtLimitAndSubjectScoped(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < MaxPushSubscriptionsPerOperator; i++ {
		if err := s.UpsertPush(ctx, PushSubscription{Subject: "operator", Endpoint: fmt.Sprintf("https://push.example/device-%d", i), P256DH: "key", Auth: "auth"}); err != nil {
			t.Fatal(err)
		}
	}
	sourceA := "https://push.example/device-0"
	sourceB := "https://push.example/device-1"
	target := PushSubscription{Subject: "operator", Endpoint: "https://push.example/current", P256DH: "new-key", Auth: "new-auth"}
	sources := []string{"https://push.example/missing", sourceA, sourceB}
	if err := s.ReplacePush(ctx, "operator", sources, target); err != nil {
		t.Fatalf("replace at limit: %v", err)
	}
	for _, endpoint := range []string{sourceA, sourceB} {
		exists, _ := s.HasPushSubscription(ctx, "operator", endpoint)
		if exists {
			t.Fatalf("same-subject source remained: %s", endpoint)
		}
	}
	if exists, _ := s.HasPushSubscription(ctx, "operator", target.Endpoint); !exists {
		t.Fatal("replacement target missing")
	}

	if err := s.UpsertPush(ctx, PushSubscription{Subject: "other", Endpoint: "https://push.example/other", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	stale := "https://push.example/stale-same-subject"
	if err := s.UpsertPush(ctx, PushSubscription{Subject: "operator", Endpoint: stale, P256DH: "stale-key", Auth: "stale-auth"}); err != nil {
		t.Fatal(err)
	}
	target.UserAgent = "updated-browser"
	if err := s.ReplacePush(ctx, "operator", []string{sourceA, stale, "https://push.example/other"}, target); err != nil {
		t.Fatalf("lost-response retry was not idempotent: %v", err)
	}
	if exists, _ := s.HasPushSubscription(ctx, "operator", stale); exists {
		t.Fatal("idempotent target did not clean remaining same-subject source")
	}
	if exists, _ := s.HasPushSubscription(ctx, "other", "https://push.example/other"); !exists {
		t.Fatal("replacement deleted another subject's source candidate")
	}

	conflicting := target
	conflicting.Auth = "different-auth"
	if err := s.ReplacePush(ctx, "operator", []string{sourceA}, conflicting); !errors.Is(err, ErrPushConflict) {
		t.Fatalf("conflicting lost-response retry = %v", err)
	}
	if err := s.ReplacePush(ctx, "operator", []string{"https://push.example/other"}, PushSubscription{Subject: "operator", Endpoint: "https://push.example/missing-target", P256DH: "key", Auth: "auth"}); !errors.Is(err, ErrPushMissing) {
		t.Fatalf("cross-subject-only sources leaked ownership: %v", err)
	}
	if err := s.ReplacePush(ctx, "operator", []string{sourceA}, PushSubscription{Subject: "operator", Endpoint: "https://push.example/other", P256DH: "key", Auth: "auth"}); !errors.Is(err, ErrPushOwnership) {
		t.Fatalf("cross-subject new endpoint = %v", err)
	}
	if err := s.ReplacePush(ctx, "operator", []string{sourceA}, PushSubscription{Subject: "operator", Endpoint: "not-https", P256DH: "key", Auth: "auth"}); !errors.Is(err, ErrInvalidPushSubscription) {
		t.Fatalf("invalid replacement = %v", err)
	}
	stillExists, _ := s.HasPushSubscription(ctx, "operator", target.Endpoint)
	if !stillExists {
		t.Fatal("invalid replacement did not roll back")
	}
	missingErr := s.ReplacePush(ctx, "operator", []string{"https://push.example/removed-by-cleanup"}, PushSubscription{Subject: "operator", Endpoint: "https://push.example/current-after-cleanup", P256DH: "key", Auth: "auth"})
	if !errors.Is(missingErr, ErrPushMissing) {
		t.Fatalf("missing replacement source was not distinguished from ownership: %v", missingErr)
	}
	invalidSources := [][]string{
		nil,
		{"https://push.example/duplicate", "https://push.example/duplicate"},
		{"not-https"},
		{strings.Repeat("x", 2049)},
	}
	overflow := make([]string, MaxPushReplacementSources+1)
	for i := range overflow {
		overflow[i] = fmt.Sprintf("https://push.example/overflow-%d", i)
	}
	invalidSources = append(invalidSources, overflow)
	for _, invalid := range invalidSources {
		if err := s.ReplacePush(ctx, "operator", invalid, target); !errors.Is(err, ErrInvalidPushSubscription) {
			t.Fatalf("invalid sources accepted: %#v, %v", invalid, err)
		}
	}
}
func TestPushBulkDeleteIsBoundedAndSubjectScopedAtLimit(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	endpoints := make([]string, MaxPushSubscriptionsPerOperator)
	for index := range endpoints {
		endpoints[index] = fmt.Sprintf("https://push.example/device-%02d", index)
		if err := s.UpsertPush(ctx, PushSubscription{Subject: "operator", Endpoint: endpoints[index], P256DH: "key", Auth: "auth"}); err != nil {
			t.Fatal(err)
		}
	}
	foreign := "https://push.example/foreign"
	if err := s.UpsertPush(ctx, PushSubscription{Subject: "other", Endpoint: foreign, P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	requested := append([]string{}, endpoints[:MaxPushDeletionEndpoints-1]...)
	requested = append(requested, foreign)
	if err := s.DeletePushEndpointsForSubject(ctx, "operator", requested); err != nil {
		t.Fatal(err)
	}
	subscriptions, err := s.PushSubscriptions(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != MaxPushSubscriptionsPerOperator-(MaxPushDeletionEndpoints-1) {
		t.Fatalf("remaining subscriptions = %d", len(subscriptions))
	}
	if exists, err := s.HasPushSubscription(ctx, "other", foreign); err != nil || !exists {
		t.Fatalf("foreign subscription changed: exists=%t err=%v", exists, err)
	}
	invalid := [][]string{
		nil,
		{endpoints[0], endpoints[0]},
		append([]string{}, endpoints[:MaxPushDeletionEndpoints]...),
	}
	invalid[2] = append(invalid[2], "https://push.example/overflow")
	for _, values := range invalid {
		if err := s.DeletePushEndpointsForSubject(ctx, "operator", values); !errors.Is(err, ErrInvalidPushSubscription) {
			t.Fatalf("invalid bulk delete accepted: %#v, %v", values, err)
		}
	}
}
func TestEnrollmentHostLimitIsTransactional(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		if err := s.CreateEnrollment(ctx, HashToken(fmt.Sprintf("token-%d", i)), fmt.Sprintf("host-%d", i), "host", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateEnrollment(ctx, HashToken("overflow"), "host-overflow", "host", now.Add(time.Minute)); !errors.Is(err, ErrHostLimit) {
		t.Fatalf("eleventh enrollment = %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM enrollments`).Scan(&count); err != nil || count != 10 {
		t.Fatalf("enrollment count = %d, %v", count, err)
	}
	s.now = func() time.Time { return now.Add(2 * time.Minute) }
	if err := s.CreateEnrollment(ctx, HashToken("replacement"), "host-replacement", "host", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("expired slot was not reclaimed: %v", err)
	}
}
func TestRestartLoadRejectsMoreThanTenKnownHosts(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 0; i < 11; i++ {
		if _, err := s.db.Exec(`INSERT INTO enrollments(token_hash,host_id,display_name,expires_at,used_at)VALUES(?,?,?,?,?)`, fmt.Sprintf("token-%d", i), fmt.Sprintf("host-%d", i), "host", now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.KnownHosts(context.Background()); !errors.Is(err, ErrHostLimit) {
		t.Fatalf("known host load = %v", err)
	}
}
func TestEnrollmentDisplayNameRejectsControlsAndInvalidStoredRows(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.CreateEnrollment(ctx, HashToken("bad"), "host", "bad\nname", time.Now().Add(time.Minute)); !errors.Is(err, ErrInvalidDisplayName) {
		t.Fatalf("control name = %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO enrollments(token_hash,host_id,display_name,expires_at,used_at)VALUES('manual','host','bad'||char(10)||'name',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.KnownHosts(ctx); !errors.Is(err, ErrInvalidDisplayName) {
		t.Fatalf("invalid loaded name = %v", err)
	}
}
func str(v string) *string { return &v }
