package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRotationSchedulerChecksContinuously(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var checks, rotations atomic.Int32
	done := make(chan struct{})
	go func() {
		RunRotationScheduler(ctx, 5*time.Millisecond, func() (bool, error) { checks.Add(1); return true, nil }, func() error {
			if rotations.Add(1) >= 2 {
				cancel()
			}
			return nil
		}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rotation scheduler did not repeat")
	}
	if checks.Load() < 2 || rotations.Load() < 2 {
		t.Fatalf("checks=%d rotations=%d", checks.Load(), rotations.Load())
	}
}
func TestServiceURLRejectsUserinfoAndCredentials(t *testing.T) {
	for _, raw := range []string{"wss://user:pass@example.test/ws", "wss://example.test/ws?token=secret", "wss://example.test/ws#fragment"} {
		if ValidateServiceURL(raw, "wss") == nil {
			t.Fatalf("unsafe URL accepted: %s", raw)
		}
	}
	if err := ValidateServiceURL("wss://example.test/ws", "wss"); err != nil {
		t.Fatal(err)
	}
}
func TestAtomicSecretWritePreservesOldCredentialOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.pem")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	renameFailure := errors.New("rename failed")
	if err := atomicWriteSecret(path, []byte("new"), func(string, string) error { return renameFailure }); !errors.Is(err, renameFailure) {
		t.Fatalf("write failure = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "old" {
		t.Fatalf("old credential changed to %q", content)
	}
	if err := writeSecret(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	content, _ = os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(content) != "new" || info.Mode().Perm() != 0600 {
		t.Fatalf("atomic credential content=%q mode=%o", content, info.Mode().Perm())
	}
}
