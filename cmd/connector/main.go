// Command connector runs one outbound-only enterprise connector.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dcolinmorgan/herdr-remote/internal/connector"
	"github.com/dcolinmorgan/herdr-remote/internal/herdr"
)

func main() {
	var cfg connector.DaemonConfig
	var socket, instance, enrollURL, rotateURL, tokenFile string
	var configuredInstances instanceFlags
	flag.StringVar(&cfg.URL, "control-plane-url", "", "connector WSS URL")
	flag.StringVar(&cfg.HostID, "host-id", "", "enrolled host UUID")
	flag.StringVar(&cfg.DisplayName, "display-name", "", "operator display label")
	flag.StringVar(&cfg.Version, "version", "0.1.0", "connector build version")
	flag.StringVar(&cfg.CertFile, "cert-file", "", "client certificate path")
	flag.StringVar(&cfg.KeyFile, "key-file", "", "client private key path")
	flag.StringVar(&cfg.CAFile, "server-ca-file", "", "control-plane server CA path")
	flag.StringVar(&socket, "herdr-socket", "", "absolute Herdr Unix socket path")
	flag.StringVar(&instance, "instance-id", "default", "Herdr instance ID")
	flag.Var(&configuredInstances, "herdr-instance", "repeatable instance_id=/absolute/socket path")
	flag.StringVar(&enrollURL, "enroll-url", "", "HTTPS enrollment endpoint (enrollment mode)")
	flag.StringVar(&rotateURL, "rotate-url", "", "HTTPS mTLS rotation endpoint")
	flag.StringVar(&tokenFile, "enrollment-token-file", "", "one-time enrollment token file path")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg.Logger = log
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if enrollURL != "" {
		if err := connector.ValidateServiceURL(enrollURL, "https"); err != nil {
			fatal("enroll-url must be HTTPS without query credentials")
		}
		if tokenFile == "" || cfg.KeyFile == "" || cfg.CertFile == "" || cfg.CAFile == "" {
			fatal("enrollment requires token, key, certificate, and server CA file paths")
		}
		enrolled, err := connector.Enroll(ctx, enrollURL, tokenFile, cfg.KeyFile, cfg.CertFile, cfg.CAFile)
		if err != nil {
			fatal(err.Error())
		}
		fmt.Fprintln(os.Stdout, enrolled.HostID)
		return
	}
	if err := connector.ValidateServiceURL(cfg.URL, "wss"); err != nil {
		fatal("control-plane-url must be WSS without query credentials")
	}
	if strings.TrimSpace(cfg.DisplayName) == "" {
		fatal("display-name is required")
	}
	if rotateURL != "" {
		if err := connector.ValidateServiceURL(rotateURL, "https"); err != nil {
			fatal("rotate-url must be HTTPS without query credentials")
		}
		go connector.RunRotationScheduler(ctx, 12*time.Hour, func() (bool, error) { return connector.CertificateExpiresSoon(cfg.CertFile, 7*24*time.Hour) }, func() error { return connector.Rotate(ctx, rotateURL, cfg.KeyFile, cfg.CertFile, cfg.CAFile) }, func(err error) { log.Error("certificate rotation failed", "error", "rotation failed") })
	}
	if len(configuredInstances) == 0 {
		configuredInstances = append(configuredInstances, instance+"="+socket)
	} else if socket != "" {
		fatal("use either herdr-socket or repeatable herdr-instance settings")
	}
	engines := map[string]*connector.Engine{}
	for _, spec := range configuredInstances {
		id, path, ok := strings.Cut(spec, "=")
		if !ok || id == "" || path == "" {
			fatal("herdr-instance must be instance_id=/absolute/socket")
		}
		if engines[id] != nil {
			fatal("duplicate Herdr instance")
		}
		local, err := herdr.NewUnixClient(path)
		if err != nil {
			fatal(err.Error())
		}
		engine, err := connector.NewEngine(connector.Config{HostID: cfg.HostID, InstanceID: id, ReconcileInterval: 30 * time.Second}, local)
		if err != nil {
			fatal(err.Error())
		}
		engines[id] = engine
	}
	daemon, err := connector.NewMultiDaemon(cfg, engines)
	if err != nil {
		fatal(err.Error())
	}
	if err = daemon.Run(ctx); err != nil && ctx.Err() == nil {
		fatal(err.Error())
	}
}
func fatal(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(2) }

type instanceFlags []string

func (i *instanceFlags) String() string         { return strings.Join(*i, ",") }
func (i *instanceFlags) Set(value string) error { *i = append(*i, value); return nil }
