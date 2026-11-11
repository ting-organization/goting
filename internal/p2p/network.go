package p2p

import "net/netip"

// Reach is where an address can be reached from.
type Reach int

const (
	ReachUnusable Reach = iota
	ReachLoopback
	ReachPrivate
	ReachPublic
)

// ReachOf classifies an address: unusable ones (unspecified, multicast,
// broadcast, the documentation ranges), this machine's, a private network's
// (with link-local and carrier-grade NAT addresses), and the public
// internet's.
func ReachOf(a netip.Addr) Reach {
	a = a.Unmap()
	switch {
	case !a.IsValid() || a.IsUnspecified() || a.IsMulticast():
		return ReachUnusable
	case a.Is4():
		o := a.As4()
		if o == [4]byte{255, 255, 255, 255} || (o[0] == 192 && o[1] == 0 && o[2] == 2) ||
			(o[0] == 198 && o[1] == 51 && o[2] == 100) || (o[0] == 203 && o[1] == 0 && o[2] == 113) {
			return ReachUnusable
		}
		if a.IsLoopback() {
			return ReachLoopback
		}
		if a.IsPrivate() || a.IsLinkLocalUnicast() || (o[0] == 100 && o[1]&0xc0 == 64) {
			return ReachPrivate
		}
		return ReachPublic
	default:
		b := a.As16()
		if b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x0d && b[3] == 0xb8 {
			return ReachUnusable
		}
		if a.IsLoopback() {
			return ReachLoopback
		}
		if b[0]&0xfe == 0xfc || (b[0] == 0xfe && b[1]&0xc0 == 0x80) {
			return ReachPrivate
		}
		return ReachPublic
	}
}

// Group is the network group of a public address: its IPv4 /16 or IPv6
// /32. An address book keeps a share of addresses for each group and a
// server gives up inbound connections by it, so that many addresses of one
// network count as one network to both. Other addresses have none.
func Group(a netip.Addr) (string, bool) {
	a = a.Unmap()
	if ReachOf(a) != ReachPublic {
		return "", false
	}
	bits := 32
	if a.Is4() {
		bits = 16
	}
	p, _ := a.Prefix(bits)
	return p.String(), true
}
