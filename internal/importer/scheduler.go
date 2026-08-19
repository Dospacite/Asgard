package importer

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Verifier re-checks stored Git credentials on a schedule.
//
// Credentials rot silently: a token expires, a deploy key is revoked from the
// repository's settings, an organisation tightens SSO enforcement. None of that
// produces an event Asgard would see. Without a periodic check the first
// symptom is a blocked release, and the dashboard actively argues against you
// in the meantime because lastUsedAt looks recent. A `git ls-remote` per
// credential transfers no objects, so checking a handful of them a few times a
// day costs nothing and moves the discovery days earlier.
type Verifier struct {
	Importer *Importer
	Interval time.Duration
	// Delay staggers the first sweep so a restart does not fire a burst of
	// outbound git connections while the control plane is still coming up.
	Delay  time.Duration
	Logger *slog.Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (v *Verifier) Start(parent context.Context) {
	if v.Importer == nil {
		return
	}
	if v.Interval <= 0 {
		v.Interval = 6 * time.Hour
	}
	if v.Delay <= 0 {
		v.Delay = time.Minute
	}
	if v.Logger == nil {
		v.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	v.cancel = cancel
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		timer := time.NewTimer(v.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		v.sweep(ctx)
		ticker := time.NewTicker(v.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				v.sweep(ctx)
			}
		}
	}()
}

func (v *Verifier) Stop() {
	if v.cancel != nil {
		v.cancel()
	}
	v.wg.Wait()
}

func (v *Verifier) sweep(ctx context.Context) {
	results, err := v.Importer.VerifyAllCredentials(ctx)
	if err != nil {
		v.Logger.Debug("credential verification sweep incomplete", "error", err)
	}
	for _, result := range results {
		switch result.Status {
		case VerifyOK:
			v.Logger.Debug("git credential verified", "credential", result.CredentialID, "refs", result.Refs)
		case VerifySkipped:
			// Worth saying once per sweep: a credential nothing can test is a
			// credential whose failure will surface mid-deploy.
			v.Logger.Info("git credential has no repository to verify against", "credential", result.CredentialID)
		default:
			v.Logger.Warn("git credential is not working", "credential", result.CredentialID, "repository", result.Repository, "error", result.Error)
		}
	}
}
