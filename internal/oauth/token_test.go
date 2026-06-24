package oauth

import (
	"testing"
	"time"
)

func TestSetExpiresAt_FromExpiresIn(t *testing.T) {
	t.Parallel()
	tok := &Token{ExpiresIn: 3600}
	before := time.Now().Unix()
	tok.SetExpiresAt()
	after := time.Now().Unix()

	// ExpiresAt should be roughly now + 3600s.
	if tok.ExpiresAt < before+3600 || tok.ExpiresAt > after+3600 {
		t.Fatalf("ExpiresAt=%d not within [%d,%d]", tok.ExpiresAt, before+3600, after+3600)
	}
}

func TestSetExpiresAt_InvalidExpiresIn_PreservesExpiresAt(t *testing.T) {
	t.Parallel()
	// Provider returned exp directly; expires_in missing.
	future := time.Now().Add(time.Hour).Unix()
	tok := &Token{ExpiresIn: 0, ExpiresAt: future}
	tok.SetExpiresAt()

	if tok.ExpiresAt != future {
		t.Fatalf("ExpiresAt was overwritten: got %d want %d", tok.ExpiresAt, future)
	}
}

func TestSetExpiresAt_NoUsableExpiry_MarksExpired(t *testing.T) {
	t.Parallel()
	// Neither expires_in nor expires_at is usable: token must be treated
	// as immediately expired rather than given a fabricated lifetime.
	tok := &Token{ExpiresIn: 0, ExpiresAt: 0}
	tok.SetExpiresAt()

	if tok.ExpiresAt != 0 {
		t.Fatalf("expected ExpiresAt=0 (forced refresh), got %d", tok.ExpiresAt)
	}
	if !tok.IsExpired() {
		t.Fatal("token with no expiry info must report IsExpired() == true")
	}
}

func TestIsExpired_NotYetExpired(t *testing.T) {
	t.Parallel()
	// Long-lived token, far from expiry: not expired.
	tok := &Token{ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if tok.IsExpired() {
		t.Fatal("fresh long-lived token should not be expired")
	}
}

func TestIsExpired_MinRefreshBufferAppliesToShortTokens(t *testing.T) {
	t.Parallel()
	// Short-lived token: expires_in/10 = 2s, but minRefreshBuffer = 30s.
	// A token that expires in 20s must already be considered expired
	// because it falls inside the 30s proactive-refresh window.
	tok := &Token{ExpiresIn: 20, ExpiresAt: time.Now().Add(20 * time.Second).Unix()}
	if !tok.IsExpired() {
		t.Fatal("short-lived token within minRefreshBuffer should be expired")
	}
}

func TestIsExpired_LargeTokenUsesPercentBuffer(t *testing.T) {
	t.Parallel()
	// Large token: expires_in/10 = 360s > minRefreshBuffer. A token that
	// expires in 100s is inside the 360s buffer → expired.
	tok := &Token{ExpiresIn: 3600, ExpiresAt: time.Now().Add(100 * time.Second).Unix()}
	if !tok.IsExpired() {
		t.Fatal("token within the percentage buffer should be expired")
	}
}
