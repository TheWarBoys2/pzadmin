package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// proxyTrust decides which forwarding headers to believe.
//
// Anyone can send X-Forwarded-For. It only means something when it was set by
// a reverse proxy the operator runs, so it is read only when the request
// arrives from one of the addresses in PZADMIN_TRUSTED_PROXIES. Everything
// that keys on the client's address depends on this: the sign-in and bad-key
// limits, the audit log and the "last used from" on sessions and keys.
type proxyTrust struct {
	prefixes []netip.Prefix
}

// parseTrustedProxies reads a comma- or space-separated list of addresses and
// CIDR ranges, such as "172.18.0.0/16, 10.0.0.5".
func parseTrustedProxies(list string) (proxyTrust, error) {
	var p proxyTrust
	for _, item := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if strings.Contains(item, "/") {
			prefix, err := netip.ParsePrefix(item)
			if err != nil {
				return proxyTrust{}, fmt.Errorf("PZADMIN_TRUSTED_PROXIES: %q is not an address or CIDR range", item)
			}
			p.prefixes = append(p.prefixes, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			return proxyTrust{}, fmt.Errorf("PZADMIN_TRUSTED_PROXIES: %q is not an address or CIDR range", item)
		}
		addr = addr.Unmap()
		p.prefixes = append(p.prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return p, nil
}

func (p proxyTrust) trusted(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range p.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// resolve returns the client's address and whether the browser used HTTPS.
//
// With no trusted proxies, that is the TCP peer and whether this connection
// is TLS. When the peer is a trusted proxy, X-Forwarded-For is walked from the
// right, the end the proxy appended to, and the first address that is not
// itself a trusted proxy is the client. Entries further left were written by
// the client and could say anything.
func (p proxyTrust) resolve(r *http.Request) (ip string, secure bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	secure = r.TLS != nil
	peer, err := netip.ParseAddr(host)
	if err != nil || !p.trusted(peer) {
		return host, secure
	}

	if proto := firstForwarded(r.Header.Values("X-Forwarded-Proto")); proto != "" {
		secure = strings.EqualFold(proto, "https")
	}

	hops := forwardedFor(r.Header.Values("X-Forwarded-For"))
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hops[i])
		if err != nil {
			// Garbage in the chain: stop at the last address we could trust.
			break
		}
		if !p.trusted(addr) {
			return addr.Unmap().String(), secure
		}
		host = addr.Unmap().String()
	}
	return host, secure
}

// forwardedFor flattens every X-Forwarded-For header into one list of hops.
func forwardedFor(values []string) []string {
	var hops []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				hops = append(hops, part)
			}
		}
	}
	return hops
}

// firstForwarded returns the protocol the nearest proxy saw. Proxies that
// append rather than replace put their own value last.
func firstForwarded(values []string) string {
	hops := forwardedFor(values)
	if len(hops) == 0 {
		return ""
	}
	return hops[len(hops)-1]
}

type clientKey struct{}

type clientInfo struct {
	ip     string
	secure bool
}

// withClient works out who the client is once per request, so every handler
// sees the same answer.
func (a *App) withClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, secure := a.proxies.resolve(r)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, clientInfo{ip: ip, secure: secure})))
	})
}

// clientIP returns the client's address as resolved by withClient.
func clientIP(r *http.Request) string {
	if c, ok := r.Context().Value(clientKey{}).(clientInfo); ok {
		return c.ip
	}
	ip, _ := proxyTrust{}.resolve(r)
	return ip
}

// requestIsSecure reports whether the browser reached us over TLS, so the
// session cookie can carry the Secure flag when it is meaningful and omit it on
// plain-HTTP LAN access where it would break login entirely.
func requestIsSecure(r *http.Request) bool {
	if c, ok := r.Context().Value(clientKey{}).(clientInfo); ok {
		return c.secure
	}
	return r.TLS != nil
}
