package enterpriseauth

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

const discoveryRecoveryGap = 5 * time.Second

// RecoveringProvider lets a Service start during an IdP outage without denying
// already-persisted application sessions. Only new login callbacks depend on it.
// Discovery retries occur on later login attempts, at most once every five
// seconds after a failed attempt; concurrent callers share one bounded request.
// A verified provider is retained through subsequent token/verification failures.
type RecoveringProvider struct {
	cfg         ProviderConfig
	mu          sync.Mutex
	provider    *Provider
	closed      bool
	flight      *discoveryFlight
	nextAttempt time.Time
	lastError   error
}

type discoveryFlight struct {
	done   chan struct{}
	cancel context.CancelFunc
	err    error
}

// NewRecoveringProvider performs the same configuration and discovery checks as
// NewProvider. Invalid configuration is fatal. A transient provider failure
// returns a degraded wrapper; cancellation of the constructor is still an abort.
func NewRecoveringProvider(ctx context.Context, cfg ProviderConfig) (*RecoveringProvider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	frozen := cloneProviderConfig(cfg)
	provider, err := NewProvider(ctx, frozen)
	if ctx.Err() != nil {
		if provider != nil {
			provider.Close()
		}
		return nil, ctx.Err()
	}
	if err != nil && !errors.Is(err, ErrProviderUnavailable) {
		return nil, err
	}
	wrapper := &RecoveringProvider{cfg: frozen, provider: provider, lastError: err}
	if err != nil {
		wrapper.nextAttempt = time.Now().Add(discoveryRecoveryGap)
	}
	return wrapper, nil
}

func cloneProviderConfig(cfg ProviderConfig) ProviderConfig {
	cfg.SigningAlgorithms = slices.Clone(cfg.SigningAlgorithms)
	cfg.AllowedReturnPaths = slices.Clone(cfg.AllowedReturnPaths)
	cfg.Outbound.AllowedOrigins = slices.Clone(cfg.Outbound.AllowedOrigins)
	if cfg.Outbound.RootCAs != nil {
		cfg.Outbound.RootCAs = cfg.Outbound.RootCAs.Clone()
	}
	return cfg
}

// Ready reports cached, verified discovery, not current IdP reachability.
func (p *RecoveringProvider) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && p.provider != nil
}

func (p *RecoveringProvider) Begin(ctx context.Context, store TransactionStore, browser string, intent LoginIntent) (Authorization, error) {
	if store == nil || !validSecret(browser) || !validIntent(intent) || !slices.Contains(p.cfg.AllowedReturnPaths, intent.ReturnPath) {
		return Authorization{}, ErrTransactionInvalid
	}
	if intent.Purpose == PurposeReauthenticate && p.cfg.Reauthentication == "" {
		return Authorization{}, ErrConfiguration
	}
	provider, err := p.acquire(ctx)
	if err != nil {
		return Authorization{}, err
	}
	return provider.Begin(ctx, store, browser, intent)
}

func (p *RecoveringProvider) Complete(ctx context.Context, store TransactionStore, browser, state, code string) (Completion, error) {
	if store == nil || !validSecret(browser) || !validSecret(state) {
		return Completion{}, ErrTransactionInvalid
	}
	provider, err := p.acquire(ctx)
	if err != nil {
		return Completion{}, err
	}
	// Exactly one invocation. The verified provider consumes state before its
	// single exchange, including when the exchange outcome becomes uncertain.
	return provider.Complete(ctx, store, browser, state, code)
}

func (p *RecoveringProvider) acquire(ctx context.Context) (*Provider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrProviderUnavailable
	}
	if p.provider != nil {
		provider := p.provider
		p.mu.Unlock()
		return provider, nil
	}
	flight := p.flight
	if flight == nil {
		if time.Now().Before(p.nextAttempt) {
			err := p.lastError
			p.mu.Unlock()
			return nil, err
		}
		// One caller cancelling its wait must not cancel discovery required by
		// other callers. NewProvider's own HTTP policy bounds this shared request;
		// Close explicitly cancels it when the owning Service shuts down.
		discoveryContext, cancel := context.WithCancel(context.Background())
		flight = &discoveryFlight{done: make(chan struct{}), cancel: cancel}
		p.flight = flight
		go p.discover(discoveryContext, flight)
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed {
			return nil, ErrProviderUnavailable
		}
		if p.provider != nil {
			return p.provider, nil
		}
		return nil, flight.err
	}
}

func (p *RecoveringProvider) discover(ctx context.Context, flight *discoveryFlight) {
	provider, err := NewProvider(ctx, p.cfg)
	flight.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		if provider != nil {
			provider.Close()
		}
		flight.err = ErrProviderUnavailable
	} else if err == nil {
		p.provider = provider
		p.lastError = nil
		flight.err = nil
	} else {
		p.lastError = err
		p.nextAttempt = time.Now().Add(discoveryRecoveryGap)
		flight.err = err
	}
	p.flight = nil
	close(flight.done)
}

// Close prevents future discovery publication and cancels any in-flight
// discovery. It does not revive or retry an uncertain authorization-code effect.
func (p *RecoveringProvider) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	flight, provider := p.flight, p.provider
	p.provider = nil
	p.mu.Unlock()
	if flight != nil {
		flight.cancel()
	}
	if provider != nil {
		provider.Close()
	}
}
