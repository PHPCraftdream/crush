package config

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
)

// providerCacheTTL is how long a previously-fetched providers.json on
// disk is considered fresh enough to short-circuit the network call.
// Override with CRUSH_PROVIDER_CACHE_TTL (e.g. "1h", "0s" to always
// re-fetch). Default chosen so a workstation that runs `crush models
// show` 50 times a day doesn't pay 50× 3s = ~2.5 minutes of latency
// over the day — refreshes happen at most once per 24h.
//
// Fork patch (orchestrator UX): bug 3 from the 2026-05-17 audit
// feedback. See CHANGELOG.fork.md (Section 4.J).
const defaultProviderCacheTTL = 24 * time.Hour

func providerCacheTTL() time.Duration {
	if v := os.Getenv("CRUSH_PROVIDER_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultProviderCacheTTL
}

type catwalkClient interface {
	GetProviders(context.Context, string) ([]catwalk.Provider, error)
}

var _ syncer[[]catwalk.Provider] = (*catwalkSync)(nil)

type catwalkSync struct {
	once       sync.Once
	result     []catwalk.Provider
	cache      cache[[]catwalk.Provider]
	client     catwalkClient
	autoupdate bool
	init       atomic.Bool
}

func (s *catwalkSync) Init(client catwalkClient, path string, autoupdate bool) {
	s.client = client
	s.cache = newCache[[]catwalk.Provider](path)
	s.autoupdate = autoupdate
	s.init.Store(true)
}

func (s *catwalkSync) Get(ctx context.Context) ([]catwalk.Provider, error) {
	if !s.init.Load() {
		panic("called Get before Init")
	}

	var throwErr error
	s.once.Do(func() {
		if !s.autoupdate {
			slog.Info("Using embedded Catwalk providers")
			s.result = embedded.GetAll()
			return
		}

		cached, etag, cachedErr := s.cache.Get()
		if len(cached) == 0 || cachedErr != nil {
			// if cached file is empty, default to embedded providers
			cached = embedded.GetAll()
		}

		// Fork patch (orchestrator UX): skip the HTTP call when the
		// on-disk cache is younger than providerCacheTTL. Saves ~1.5s
		// per `crush models show` after the first call of the day.
		if age, ageErr := s.cache.Age(); ageErr == nil && age < providerCacheTTL() && len(cached) > 0 && cachedErr == nil {
			slog.Debug("Catwalk providers cache fresh, skipping fetch", "age", age, "ttl", providerCacheTTL())
			s.result = cached
			return
		}

		slog.Info("Fetching providers from Catwalk")
		result, err := s.client.GetProviders(ctx, etag)
		if errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("Catwalk providers not updated in time")
			s.result = cached
			return
		}
		if errors.Is(err, catwalk.ErrNotModified) {
			slog.Info("Catwalk providers not modified")
			s.result = cached
			return
		}
		if err != nil {
			// On error, fall back to cached (which defaults to embedded if empty).
			s.result = cached
			return
		}
		if len(result) == 0 {
			s.result = cached
			throwErr = errors.New("empty providers list from catwalk")
			return
		}

		s.result = result
		throwErr = s.cache.Store(result)
	})
	return s.result, throwErr
}
