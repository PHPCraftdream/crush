package tools

// ssrf_guard.go is an SSRF defense-in-depth gate shared by the download
// and fetch tools. It refuses any HTTP(S) request whose resolved
// destination falls into loopback / RFC1918-private / link-local /
// unique-local / unspecified ranges — the ranges that host cloud
// instance-metadata services (AWS 169.254.169.254, IPv6 fd00:ec2::254),
// internal admin panels, and anything else a prompt-injected or
// malicious model could otherwise exfiltrate through final_text.
//
// The single permission layer is the only other gate, and in non-
// interactive `crush run` / web contexts it auto-approves, so this guard
// is the thing that actually stops a `http://169.254.169.254/latest/
// meta-data/` request from ever leaving the process.
//
// DNS rebinding / TOCTOU: the guard runs at net.Dialer.Control, which
// fires AFTER DNS resolution but BEFORE the TCP connect syscall, and it
// receives the exact resolved IP the kernel is about to dial. That
// closes both the trivial bypass (a hostname resolving straight to
// 169.254.169.254) and the rebinding class (a hostname returning a
// public IP during a pre-check but a private IP at connect time):
// because Control inspects the dial address rather than re-resolving,
// there is no second lookup that can diverge from the first.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// errSSRFBlocked is the sentinel a forbidden dial resolves to. It is
// wrapped into the tool's error path; tests assert on it via
// errors.Is instead of string-matching.
var errSSRFBlocked = errors.New("blocked by SSRF guard: destination resolves to a loopback/private/link-local/metadata range")

// isForbiddenSSRFAddress reports whether ip is in a range the guard
// rejects. The checks deliberately overlap with the well-known cloud
// metadata endpoints:
//
//   - IsLoopback: 127.0.0.0/8 and ::1.
//   - IsPrivate: 10/8, 172.16/12, 192.168/16 (RFC1918) and IPv6
//     unique-local fc00::/7. fc00::/7 also covers the AWS IMDS IPv6
//     endpoint fd00:ec2::254.
//   - IsLinkLocalUnicast: 169.254.0.0/16 and fe80::/10. 169.254.0.0/16
//     covers the IPv4 metadata endpoint 169.254.169.254 used by AWS,
//     GCP, Azure, Alibaba, Oracle and others.
//   - IsLinkLocalMulticast and IsUnspecified (0.0.0.0, ::): not real
//     HTTP targets, rejected for completeness.
//
// A nil ip is treated as forbidden: the caller is expected to have
// parsed one already, and an unparseable dial address is itself the
// danger signal the guard exists to refuse.
func isForbiddenSSRFAddress(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

// ssrfControlHook is the net.Dialer.Control callback. For "tcp"/"udp"
// the address handed to Control is "<resolved-ip>:<port>"; we parse out
// the IP and reject forbidden ranges. Returning an error here aborts
// the dial before any bytes hit the wire, and the error surfaces through
// http.Client.Do into the tool.
func ssrfControlHook(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparseable dial address %q", errSSRFBlocked, address)
	}
	ip := net.ParseIP(host)
	if isForbiddenSSRFAddress(ip) {
		if ip == nil {
			return fmt.Errorf("%w: non-IP dial address %q", errSSRFBlocked, address)
		}
		return fmt.Errorf("%w: %s", errSSRFBlocked, ip)
	}
	return nil
}

// applySSRFGuard installs the dial-time SSRF check on transport unless
// allowPrivate is true. When allowPrivate is true the transport dials
// with a plain dialer (no Control hook), which is the explicit local-dev
// / self-hosted escape hatch for operators who need to fetch from
// 127.0.0.1 or an internal host. Production wiring passes false.
//
// Proxy bypass (found by an independent @oh review of the guard itself):
// http.DefaultTransport.Clone() inherits Proxy: ProxyFromEnvironment.
// Control only ever sees the address actually dialed — if HTTP_PROXY/
// HTTPS_PROXY is set, that address is the proxy, not the request's real
// target, so a guarded client would tunnel straight past the check to
// whatever host the request line names (including a forbidden one). When
// the guard is active we disable Transport-level proxying entirely
// rather than silently trusting an unvalidated proxy hop; a caller that
// truly needs both an egress proxy AND fetches from private ranges
// should build its own client with allowPrivate=true instead of relying
// on the guarded default.
func applySSRFGuard(transport *http.Transport, allowPrivate bool) {
	if allowPrivate {
		transport.DialContext = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		return
	}
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   ssrfControlHook,
	}).DialContext
}

// NewSSRFGuardedClient builds the *http.Client used by the download and
// fetch tools when no explicit client is injected. The transport clones
// http.DefaultTransport (inheriting proxy / TLS defaults), is tuned with
// the same idle-connection limits the tools already used, and — unless
// allowPrivate is true — blocks dials to forbidden ranges via Control.
//
// allowPrivate exists for legitimate local-dev / self-hosted use; the
// tool constructors take an *http.Client argument precisely so a caller
// that needs loopback can inject one built with allowPrivate=true rather
// than the guarded default.
func NewSSRFGuardedClient(timeout time.Duration, allowPrivate bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second
	applySSRFGuard(transport, allowPrivate)
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}
