package api

import (
	"context"
	"crypto/x509"
	"time"
)

// SetAppLogsTimeout shortens AppLogs' read bound for a test and returns the
// function restoring it.
func SetAppLogsTimeout(d time.Duration) func() {
	old := appLogsTimeout
	appLogsTimeout = d
	return func() { appLogsTimeout = old }
}

// SetDomainProbe replaces DomainStatus's network access for a test.
func (s *Service) SetDomainProbe(
	lookup func(ctx context.Context, host string) ([]string, error),
	certs func(ctx context.Context, addr, serverName string) ([]*x509.Certificate, error),
	roots *x509.CertPool, now func() time.Time,
) {
	s.domains = domainProbe{lookup: lookup, certs: certs, roots: roots, now: now}
}

// RedactBackupError exposes the backup error redaction.
var RedactBackupError = redactBackupError
