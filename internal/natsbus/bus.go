// Package natsbus connects team-registry to NATS: request subscriptions and
// JetStream publication of outbox rows.
package natsbus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pandops/flowai-team-registry/internal/config"
	"github.com/pandops/flowai-team-registry/internal/team"
)

// InboxPrefix is team-registry's own reply inbox prefix.
const InboxPrefix = "_INBOX.team-registry"

// Options returns the connection options for cfg: TLS against the
// configured CA, the service's own user JWT and seed, its own inbox prefix,
// and unlimited reconnects, also when the first connect fails.
func Options(cfg config.NATS, logger *slog.Logger) ([]nats.Option, error) {
	creds, err := os.ReadFile(cfg.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("nats credentials: %w", err)
	}
	if _, err := jwt.ParseDecoratedJWT(creds); err != nil || len(creds) == 0 {
		return nil, errors.New("nats credentials: no user JWT")
	}
	if _, err := jwt.ParseDecoratedUserNKey(creds); err != nil {
		return nil, errors.New("nats credentials: no user seed")
	}
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("nats CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("nats CA: no certificate")
	}
	prefix := cfg.InboxPrefix
	if prefix == "" {
		prefix = InboxPrefix
	}
	return []nats.Option{
		nats.Name("team-registry"),
		nats.UserCredentials(cfg.CredentialsFile),
		nats.Secure(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}),
		nats.CustomInboxPrefix(prefix),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.RetryOnFailedConnect(true),
		nats.Timeout(2 * time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.Warn("broker connection lost", "error", err.Error())
			}
		}),
		nats.ReconnectHandler(func(*nats.Conn) { logger.Info("broker connection restored") }),
	}, nil
}

// Bus is the live broker connection.
type Bus struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	timeout time.Duration
	logger  *slog.Logger
}

// Connect opens the connection; it keeps retrying in the background, so an
// unreachable broker leaves the service running and not ready.
func Connect(cfg config.NATS, logger *slog.Logger) (*Bus, error) {
	opts, err := Options(cfg, logger)
	if err != nil {
		return nil, err
	}
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Bus{nc: nc, js: js, timeout: timeout, logger: logger}, nil
}

// Connected reports whether the connection is up.
func (b *Bus) Connected() bool { return b.nc.IsConnected() }

// Ready returns an error while the connection is down.
func (b *Bus) Ready(context.Context) error {
	if !b.nc.IsConnected() {
		return errors.New("broker not connected")
	}
	return nil
}

// Publish publishes to JetStream with the Nats-Msg-Id and waits for the
// acknowledgement.
func (b *Bus) Publish(ctx context.Context, subject, msgID string, headers map[string]string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	msg := nats.NewMsg(subject)
	for k, v := range headers {
		msg.Header.Set(k, v)
	}
	msg.Data = data
	_, err := b.js.PublishMsg(ctx, msg, jetstream.WithMsgID(msgID))
	return err
}

// Serve subscribes the handler on every subject in the team-registry queue
// group and answers each request with the status header and the body. The
// subscriptions end when ctx ends.
func (b *Bus) Serve(ctx context.Context, subjects []string, h *team.Handler) error {
	var subs []*nats.Subscription
	for _, s := range subjects {
		sub, err := b.nc.QueueSubscribe(s, team.QueueGroup, func(m *nats.Msg) {
			if m.Reply == "" {
				return
			}
			header := map[string]string{}
			for k, v := range m.Header {
				if len(v) > 0 {
					header[k] = v[0]
				}
			}
			rctx, cancel := context.WithTimeout(ctx, b.timeout)
			rep := h.Handle(rctx, team.Request{Subject: m.Subject, Header: header, Data: m.Data})
			cancel()
			out := nats.NewMsg(m.Reply)
			for k, v := range rep.Header {
				out.Header.Set(k, v)
			}
			out.Header.Set("status", strconv.Itoa(rep.Status))
			out.Data = rep.Body
			if err := m.RespondMsg(out); err != nil {
				b.logger.Warn("reply failed", "subject", m.Subject, "error", err.Error())
			}
		})
		if err != nil {
			for _, s := range subs {
				_ = s.Unsubscribe()
			}
			return err
		}
		subs = append(subs, sub)
	}
	go func() {
		<-ctx.Done()
		for _, s := range subs {
			_ = s.Drain()
		}
	}()
	return nil
}

// Close drains and closes the connection.
func (b *Bus) Close() {
	if err := b.nc.Drain(); err != nil {
		b.nc.Close()
	}
}
