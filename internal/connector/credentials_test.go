package connector

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testPrivateKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
}

func testCertificateForKey(t *testing.T, key *ecdsa.PrivateKey, serial int64) []byte {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "test-connector"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

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

func TestEnsureCertificatePreservesValidRotatedState(t *testing.T) {
	directory := t.TempDir()
	stateDirectory := filepath.Join(directory, "state")
	if err := os.Mkdir(stateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	key, keyPEM := testPrivateKey(t)
	keyFile := filepath.Join(directory, "client.key")
	initial := filepath.Join(directory, "initial.crt")
	mutable := filepath.Join(stateDirectory, "client.crt")
	initialCertificate := testCertificateForKey(t, key, 1)
	rotatedCertificate := testCertificateForKey(t, key, 2)
	if err := os.WriteFile(keyFile, keyPEM, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initial, initialCertificate, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mutable, rotatedCertificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCertificate(mutable, initial, keyFile); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(mutable)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(rotatedCertificate) {
		t.Fatal("valid rotated state certificate was overwritten")
	}
}

func TestEnsureCertificateMigratesMissingState(t *testing.T) {
	directory := t.TempDir()
	key, keyPEM := testPrivateKey(t)
	certificate := testCertificateForKey(t, key, 1)
	initial := filepath.Join(directory, "initial.crt")
	keyFile := filepath.Join(directory, "client.key")
	mutable := filepath.Join(directory, "state.crt")
	if err := os.WriteFile(initial, certificate, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0400); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCertificate(mutable, initial, keyFile); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(mutable)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(certificate) {
		t.Fatal("initial certificate was not migrated")
	}
}

func TestEnsureCertificateReplacesStateForNewKeyReenrollment(t *testing.T) {
	directory := t.TempDir()
	stateDirectory := filepath.Join(directory, "state")
	if err := os.Mkdir(stateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	oldKey, _ := testPrivateKey(t)
	newKey, newKeyPEM := testPrivateKey(t)
	mutable := filepath.Join(stateDirectory, "client.crt")
	initial := filepath.Join(directory, "initial.crt")
	keyFile := filepath.Join(directory, "client.key")
	oldCertificate := testCertificateForKey(t, oldKey, 1)
	newCertificate := testCertificateForKey(t, newKey, 2)
	if err := os.WriteFile(mutable, oldCertificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initial, newCertificate, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, newKeyPEM, 0400); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCertificate(mutable, initial, keyFile); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(mutable)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(mutable)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newCertificate) || info.Mode().Perm() != 0600 {
		t.Fatalf("re-enrolled certificate was not installed atomically: mode=%o", info.Mode().Perm())
	}
}

func TestEnsureCertificateRejectsInvalidInitialWithoutDestroyingState(t *testing.T) {
	directory := t.TempDir()
	oldKey, _ := testPrivateKey(t)
	_, newKeyPEM := testPrivateKey(t)
	mutable := filepath.Join(directory, "client.crt")
	initial := filepath.Join(directory, "initial.crt")
	keyFile := filepath.Join(directory, "client.key")
	oldCertificate := testCertificateForKey(t, oldKey, 1)
	if err := os.WriteFile(mutable, oldCertificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initial, []byte("not a certificate"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, newKeyPEM, 0400); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCertificate(mutable, initial, keyFile); err == nil {
		t.Fatal("invalid initial certificate was accepted")
	}
	got, err := os.ReadFile(mutable)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(oldCertificate) {
		t.Fatal("invalid initial certificate destroyed existing state")
	}
}

func TestEnsureCertificateAtomicFailurePreservesState(t *testing.T) {
	directory := t.TempDir()
	oldKey, _ := testPrivateKey(t)
	newKey, newKeyPEM := testPrivateKey(t)
	mutable := filepath.Join(directory, "client.crt")
	initial := filepath.Join(directory, "initial.crt")
	keyFile := filepath.Join(directory, "client.key")
	oldCertificate := testCertificateForKey(t, oldKey, 1)
	newCertificate := testCertificateForKey(t, newKey, 2)
	if err := os.WriteFile(mutable, oldCertificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initial, newCertificate, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, newKeyPEM, 0400); err != nil {
		t.Fatal(err)
	}
	renameFailure := errors.New("rename failed")
	err := ensureCertificate(mutable, initial, keyFile, func(string, string) error { return renameFailure })
	if !errors.Is(err, renameFailure) {
		t.Fatalf("replacement failure = %v", err)
	}
	got, readErr := os.ReadFile(mutable)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(oldCertificate) {
		t.Fatal("atomic replacement failure destroyed existing state")
	}
}
