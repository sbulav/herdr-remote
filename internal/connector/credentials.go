package connector

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type EnrollmentResponse struct {
	HostID           string    `json:"host_id"`
	CertificatePEM   string    `json:"certificate_pem"`
	CACertificatePEM string    `json:"ca_certificate_pem"`
	NotAfter         time.Time `json:"not_after"`
}

// Enroll generates a local P-256 key and sends only its CSR and one-time token.
func Enroll(ctx context.Context, endpoint, tokenFile, keyFile, certFile, serverCA string) (EnrollmentResponse, error) {
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{}}, key)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	body, _ := json.Marshal(map[string]string{"token": string(bytes.TrimSpace(token)), "csr_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))})
	client, err := bootstrapHTTPClient(serverCA)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return EnrollmentResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return EnrollmentResponse{}, errors.New("enrollment rejected")
	}
	var out EnrollmentResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
		return out, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return out, err
	}
	if err := writeSecret(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})); err != nil {
		return out, err
	}
	if err := writeSecret(certFile, []byte(out.CertificatePEM)); err != nil {
		return out, err
	}
	_ = os.Remove(tokenFile)
	return out, nil
}

func Rotate(ctx context.Context, endpoint, keyFile, certFile, serverCA string) error {
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return errors.New("invalid connector key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{}}, key)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"csr_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))})
	client, err := mTLSHTTPClient(certFile, keyFile, serverCA)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return errors.New("certificate rotation rejected")
	}
	var out EnrollmentResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
		return err
	}
	return writeSecret(certFile, []byte(out.CertificatePEM))
}

// EnsureCertificate migrates an enrolled certificate into its writable state
// location. A state certificate matching the configured key is preserved. A
// stale state certificate is replaced only when the initial certificate
// matches that key.
func EnsureCertificate(certFile, initialCertFile, keyFile string) error {
	return ensureCertificate(certFile, initialCertFile, keyFile, os.Rename)
}

func ensureCertificate(certFile, initialCertFile, keyFile string, rename func(string, string) error) error {
	if certFile == "" {
		return errors.New("client certificate path is required")
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}
	stateCertificate, stateErr := os.ReadFile(certFile)
	if stateErr == nil {
		if _, err := tls.X509KeyPair(stateCertificate, keyPEM); err == nil {
			return nil
		}
	} else if !errors.Is(stateErr, os.ErrNotExist) {
		return stateErr
	}
	if initialCertFile == "" {
		return errors.New("client certificate does not match the configured key and no initial certificate was configured")
	}
	certificate, err := os.ReadFile(initialCertFile)
	if err != nil {
		return err
	}
	if _, err := tls.X509KeyPair(certificate, keyPEM); err != nil {
		return errors.New("initial connector certificate does not match the configured key")
	}
	return atomicWriteSecret(certFile, certificate, rename)
}

func CertificateExpiresSoon(path string, within time.Duration) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return false, errors.New("invalid connector certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, err
	}
	return time.Until(cert.NotAfter) <= within, nil
}
func RunRotationScheduler(ctx context.Context, interval time.Duration, needsRotation func() (bool, error), rotate func() error, onError func(error)) {
	if interval <= 0 {
		interval = 12 * time.Hour
	}
	run := func() {
		needed, err := needsRotation()
		if err == nil && needed {
			err = rotate()
		}
		if err != nil && onError != nil {
			onError(err)
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
func ValidateServiceURL(raw, scheme string) error {
	value, err := url.Parse(raw)
	if err != nil || value.Scheme != scheme || value.Host == "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" {
		return errors.New("service URL must use the required scheme without userinfo, query, or fragment")
	}
	return nil
}
func bootstrapHTTPClient(caFile string) (*http.Client, error) {
	roots, err := loadRoots(caFile)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}, Timeout: 30 * time.Second}, nil
}
func mTLSHTTPClient(certFile, keyFile, caFile string) (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	roots, err := loadRoots(caFile)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{cert}}}, Timeout: 30 * time.Second}, nil
}
func loadRoots(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, errors.New("invalid CA file")
	}
	return p, nil
}
func writeSecret(path string, data []byte) error {
	return atomicWriteSecret(path, data, os.Rename)
}
func atomicWriteSecret(path string, data []byte, rename func(string, string) error) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	fail := func(err error) error { _ = temporary.Close(); return err }
	if err = temporary.Chmod(0600); err != nil {
		return fail(err)
	}
	if _, err = temporary.Write(data); err != nil {
		return fail(err)
	}
	if err = temporary.Sync(); err != nil {
		return fail(err)
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	if err = rename(temporaryPath, path); err != nil {
		return err
	}
	keep = true
	if directoryHandle, openErr := os.Open(directory); openErr == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}
