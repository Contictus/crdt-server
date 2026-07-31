package gateway

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Knowing which client a connection belongs to is the difference between "the
// server is refusing connections" and "this address is opening thousands of
// them". Until now nothing in this server read a peer address at all, so an
// incident had no answer to "where is it coming from".
//
// The address has to be established carefully, because the header that usually
// carries it is written by the client. X-Forwarded-For is a request header like
// any other: anyone can send one saying anything. Trusting it unconditionally
// would mean an attacker picks their own identity, and a per-address limit that
// an attacker can opt out of is worse than none - it costs the work and gives
// the false impression of a control.
//
// So the header is consulted only when the machine we are actually talking to
// is a proxy the operator named. Everything else uses the socket's peer
// address, which cannot be forged over an established TCP connection.

// Proxies is the set of addresses whose X-Forwarded-For header is believed.
// The zero value trusts nobody, which is the right default: a server reached
// directly must ignore the header entirely.
type Proxies struct {
	nets []netip.Prefix
}

// ParseProxies builds the trusted set from CIDR blocks or bare addresses.
//
// The two shorthands are the two real deployments: "loopback" for a reverse
// proxy on the same host, and "private" for an ingress controller inside a
// cluster network. Naming them beats every operator writing out the same three
// RFC 1918 blocks and one of them getting it wrong.
func ParseProxies(entries []string) (Proxies, error) {
	var p Proxies
	add := func(cidrs ...string) {
		for _, c := range cidrs {
			p.nets = append(p.nets, netip.MustParsePrefix(c))
		}
	}
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		switch strings.ToLower(entry) {
		case "loopback":
			add("127.0.0.0/8", "::1/128")
			continue
		case "private":
			add("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
				"127.0.0.0/8", "::1/128", "fc00::/7", "fe80::/10")
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			p.nets = append(p.nets, prefix)
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return Proxies{}, fmt.Errorf("bad trusted proxy %q: not an address, a CIDR block, %q or %q", raw, "loopback", "private")
		}
		p.nets = append(p.nets, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return p, nil
}

// Any reports whether any proxy is trusted at all.
func (p Proxies) Any() bool { return len(p.nets) > 0 }

func (p Proxies) trusts(addr netip.Addr) bool {
	// An IPv4 address arriving over a dual-stack listener is ::ffff:a.b.c.d,
	// which matches no IPv4 prefix until it is unmapped.
	addr = addr.Unmap()
	for _, n := range p.nets {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

// ClientIP returns the address to attribute this request to.
//
// With no trusted proxies it is always the socket's peer. With some, and when
// the peer is one of them, it is the rightmost entry in X-Forwarded-For that is
// not itself trusted - the last address a machine we trust actually saw. Taking
// the leftmost entry instead is the classic mistake: that end of the list is
// the part the client wrote.
func (p Proxies) ClientIP(r *http.Request) string {
	peer := peerAddr(r.RemoteAddr)
	if !p.Any() {
		return peer.String()
	}
	if !peer.IsValid() || !p.trusts(peer) {
		return peer.String()
	}
	forwarded := r.Header.Values("X-Forwarded-For")
	var hops []string
	for _, h := range forwarded {
		for part := range strings.SplitSeq(h, ",") {
			if v := strings.TrimSpace(part); v != "" {
				hops = append(hops, v)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.Trim(hops[i], "[]"))
		if err != nil {
			// A malformed hop means the chain cannot be reasoned about past this
			// point. Stop here rather than skipping it and attributing the
			// request to something further left, which is the half the client
			// controls.
			return peer.String()
		}
		if !p.trusts(addr) {
			return addr.Unmap().String()
		}
	}
	// Every hop is trusted, which happens when a proxy adds its own address and
	// there was no client entry. The peer is as good as it gets.
	return peer.String()
}

func peerAddr(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}
