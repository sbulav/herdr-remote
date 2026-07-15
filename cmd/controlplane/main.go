// Command controlplane runs separate browser and connector listeners.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dcolinmorgan/herdr-remote/internal/auth"
	"github.com/dcolinmorgan/herdr-remote/internal/controlplane"
	"github.com/dcolinmorgan/herdr-remote/internal/enrollment"
	"github.com/dcolinmorgan/herdr-remote/internal/push"
	"github.com/dcolinmorgan/herdr-remote/internal/store"
)

func main() {
	var browserAddr, connectorAddr, origin, dbPath, staticDir, sessionSecret, caCert, caKey, tlsCert, tlsKey, clientCA, proxyCIDRs, issuer, audience, subject, mfa, vapidPublic, vapidPrivateFile, vapidSubscriber string
	flag.StringVar(&browserAddr, "browser-listen", "127.0.0.1:8080", "reverse-proxy browser HTTP listener")
	flag.StringVar(&connectorAddr, "connector-listen", ":8443", "connector mTLS listener")
	flag.StringVar(&origin, "origin", "", "exact browser HTTPS origin")
	flag.StringVar(&dbPath, "database", "", "SQLite database path")
	flag.StringVar(&staticDir, "static-dir", "", "PWA static directory")
	flag.StringVar(&sessionSecret, "session-secret-file", "", "session secret file path")
	flag.StringVar(&caCert, "private-ca-cert-file", "", "connector issuing CA certificate path")
	flag.StringVar(&caKey, "private-ca-key-file", "", "connector issuing CA private key path")
	flag.StringVar(&tlsCert, "connector-tls-cert-file", "", "connector listener certificate path")
	flag.StringVar(&tlsKey, "connector-tls-key-file", "", "connector listener key path")
	flag.StringVar(&clientCA, "connector-client-ca-file", "", "trusted connector client CA path")
	flag.StringVar(&proxyCIDRs, "trusted-proxy-cidrs", "127.0.0.0/8,::1/128", "comma-separated proxy CIDRs")
	flag.StringVar(&issuer, "oidc-issuer", "", "exact trusted issuer header value")
	flag.StringVar(&audience, "oidc-audience", "", "exact trusted audience header value")
	flag.StringVar(&subject, "oidc-subject", "", "single allowed subject")
	flag.StringVar(&mfa, "oidc-mfa", "", "required MFA assurance value")
	flag.StringVar(&vapidPublic, "vapid-public-key", "", "VAPID public key")
	flag.StringVar(&vapidPrivateFile, "vapid-private-key-file", "", "VAPID private key file path")
	flag.StringVar(&vapidSubscriber, "vapid-subscriber", "", "VAPID mailto/contact URI")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	st, err := store.Open(dbPath)
	if err != nil {
		fatal(err)
	}
	defer st.Close()
	if recovered, err := st.RecoverIncomplete(context.Background()); err != nil {
		fatal(err)
	} else if recovered > 0 {
		log.Warn("recovered incomplete actions", "count", recovered)
	}
	sessions, err := auth.NewSessions(sessionSecret)
	if err != nil {
		fatal(err)
	}
	proxy, err := auth.NewProxy(auth.ProxyConfig{CIDRs: split(proxyCIDRs), Headers: auth.HeaderConfig{Issuer: "X-OIDC-Issuer", Audience: "X-OIDC-Audience", Subject: "X-OIDC-Subject", Assurance: "X-OIDC-Assurance"}, Expected: auth.Identity{Issuer: issuer, Audience: audience, Subject: subject, Assurance: mfa}})
	if err != nil {
		fatal(err)
	}
	enroll, err := enrollment.Load(st, caCert, caKey)
	if err != nil {
		fatal(err)
	}
	metrics := &controlplane.Metrics{}
	hub, err := controlplane.NewHub(st, log, metrics)
	if err != nil {
		fatal(err)
	}
	var pushService *push.Service
	if vapidPrivateFile != "" || vapidPublic != "" || vapidSubscriber != "" {
		if vapidPrivateFile == "" || vapidPublic == "" || vapidSubscriber == "" {
			fatal(fmt.Errorf("all VAPID settings are required together"))
		}
		private, err := os.ReadFile(vapidPrivateFile)
		if err != nil {
			fatal(err)
		}
		pushService = &push.Service{Store: st, Sender: &push.VAPIDSender{PublicKey: vapidPublic, PrivateKey: strings.TrimSpace(string(private)), Subscriber: vapidSubscriber, TTL: 60}}
	}
	server, err := controlplane.NewServer(controlplane.ServerConfig{Origin: origin, StaticDir: staticDir, Proxy: proxy, Sessions: sessions, Store: st, Enrollment: enroll, Push: pushService, OperatorSubject: subject, Logger: log, Metrics: metrics}, hub)
	if err != nil {
		fatal(err)
	}
	browser := &http.Server{Addr: browserAddr, Handler: server.BrowserHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024}
	connectorTLS, err := connectorTLSConfig(clientCA)
	if err != nil {
		fatal(err)
	}
	connectors := &http.Server{Addr: connectorAddr, Handler: server.ConnectorHandler(), TLSConfig: connectorTLS, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024}
	retentionCtx, stopRetention := context.WithCancel(context.Background())
	defer stopRetention()
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			if err := st.Retain(retentionCtx, 90*24*time.Hour); err != nil {
				log.Error("audit retention failed", "error", "storage unavailable")
			}
			select {
			case <-retentionCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	errc := make(chan error, 2)
	go func() { log.Info("browser listener started", "address", browserAddr); errc <- browser.ListenAndServe() }()
	go func() {
		log.Info("connector listener started", "address", connectorAddr)
		errc <- connectors.ListenAndServeTLS(tlsCert, tlsKey)
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errc:
		if err != nil && err != http.ErrServerClosed {
			fatal(err)
		}
	case <-ctx.Done():
	}
	shutdown, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	_ = browser.Shutdown(shutdown)
	_ = connectors.Shutdown(shutdown)
}
func connectorTLSConfig(caPath string) (*tls.Config, error) {
	b, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("invalid connector client CA")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, NextProtos: []string{"http/1.1"}}, nil
}
func split(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(2) }
