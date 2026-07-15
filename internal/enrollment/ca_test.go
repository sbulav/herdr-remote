package enrollment

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/dcolinmorgan/herdr-remote/internal/store"
)

func TestOneTimeCSRHostMappingAndRevocation(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ca, key := testCA(t)
	svc := New(st, ca, key)
	tok, err := svc.CreateToken(context.Background(), "host")
	if err != nil {
		t.Fatal(err)
	}
	clientKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, clientKey)
	if _, err := svc.Enroll(context.Background(), tok.Token, []byte("invalid CSR")); err == nil {
		t.Fatal("invalid CSR accepted")
	}
	cert, err := svc.Enroll(context.Background(), tok.Token, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	if err != nil {
		t.Fatal(err)
	}
	if cert.HostID != tok.HostID {
		t.Fatal("certificate mapped to wrong host")
	}
	block, _ := pem.Decode([]byte(cert.CertificatePEM))
	parsed, _ := x509.ParseCertificate(block.Bytes)
	record, err := st.CertificateByFingerprint(context.Background(), Fingerprint(parsed))
	if err != nil || record.HostID != tok.HostID {
		t.Fatalf("mapping missing: %#v %v", record, err)
	}
	if _, err := svc.Enroll(context.Background(), tok.Token, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})); err == nil {
		t.Fatal("enrollment token reused")
	}
	if _, err := svc.Rotate(context.Background(), tok.HostID, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})); err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(context.Background(), tok.HostID); err != nil {
		t.Fatal(err)
	}
	record, _ = st.CertificateByFingerprint(context.Background(), Fingerprint(parsed))
	if !record.Revoked {
		t.Fatal("certificate not revoked")
	}
	for _, kind := range []string{"enrollment.created", "enrollment.completed", "certificate.rotated", "certificate.revoked"} {
		count, err := st.CountAuditEvents(context.Background(), kind)
		if err != nil || count != 1 {
			t.Fatalf("audit %s = %d, %v", kind, count, err)
		}
	}
}

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return ca, key
}
