package endpointcache

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// maxHostLength is the longest hostname DNS allows, without the root dot.
const maxHostLength = 253

// Derivation refusals. ErrPublicSuffix is the public_suffix outcome, where the
// seed's domain is itself a public suffix, or is a single label the list
// treats as one, or a derived host would be a public suffix or fall under a
// different registrable domain than the seed's, so anyone could register the
// node's name there. The derivation then yields no address. ErrSeedEndpoint
// means the seed does not meet the endpoint grammar, has no host or no port,
// ends in an all-digit label, or the derived name would not be a legal host.
// Neither error echoes the seed.
var (
	ErrPublicSuffix = errors.New("seed domain is a public suffix")
	ErrSeedEndpoint = errors.New("seed is not a valid endpoint to derive node addresses from")
)

// Deriver turns node names into endpoint addresses under one seed. It holds
// the seed's domain, which is empty for a bare-name or IP seed, the
// registrable domain of that domain, and the seed's port. A Deriver is
// immutable and safe for concurrent use.
type Deriver struct {
	domain      string
	registrable string
	port        int
}

// NewDeriver prepares address derivation for a seed written as host:port, the
// form the config layer resolves every endpoint to. A seed without a port, or
// without a host, is refused with ErrSeedEndpoint, and so is a seed whose last
// label is all digits, because no top-level domain is numeric and such a name
// is a malformed address and not a name. When the seed is a fully qualified
// name, meaning it holds a dot and is not an IP literal, nodes take the seed's
// domain, which is everything after its first label. The domain must pass
// publicsuffix.EffectiveTLDPlusOne, and so must a name under it, since a
// wildcard rule can make the domain registrable and the names beneath it
// suffixes. Otherwise the error wraps ErrPublicSuffix. When the seed is a bare
// name or an IP literal, nodes keep their bare name. Every address uses the
// seed's port.
func NewDeriver(seed string) (*Deriver, error) {
	if _, _, err := net.SplitHostPort(seed); err != nil {
		return nil, ErrSeedEndpoint
	}
	host, port, ok := config.NormalizeEndpoint(seed)
	if !ok || host == "" {
		return nil, ErrSeedEndpoint
	}
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return &Deriver{port: port}, nil
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" {
			return nil, ErrSeedEndpoint
		}
	}
	if allDigits(labels[len(labels)-1]) {
		return nil, ErrSeedEndpoint
	}
	_, domain, _ := strings.Cut(host, ".")
	registrable, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return nil, ErrPublicSuffix
	}
	d := &Deriver{domain: domain, registrable: registrable, port: port}
	if !d.underRegistrable(placeholderNode + "." + domain) {
		return nil, ErrPublicSuffix
	}
	return d, nil
}

// placeholderNode is a valid node name that NewDeriver derives under the
// seed's domain, so a seed that no node can derive under fails once, early.
const placeholderNode = "n"

// underRegistrable reports whether host has a registrable domain and it is
// the seed's. The list is lower case, and a node keeps its own spelling, so
// the check reads the lower-cased host.
func (d *Deriver) underRegistrable(host string) bool {
	registrable, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(host))
	return err == nil && registrable == d.registrable
}

// Address returns the host:port a node answers on. The node name must pass
// ValidateNodeName, and a failure wraps its tier error so the caller can tell
// a dropped node from a rejected source. A derived host must keep the seed's
// registrable domain, and otherwise the error wraps ErrPublicSuffix. The
// result meets the endpoints list grammar, and its host is at most 253
// characters.
func (d *Deriver) Address(node string) (string, error) {
	if err := ValidateNodeName(node); err != nil {
		return "", err
	}
	host := node
	if d.domain != "" {
		host = node + "." + d.domain
		if !d.underRegistrable(host) {
			return "", ErrPublicSuffix
		}
	}
	if len(host) > maxHostLength {
		return "", fmt.Errorf("%w: derived name is longer than %d characters", ErrSeedEndpoint, maxHostLength)
	}
	address := net.JoinHostPort(host, strconv.Itoa(d.port))
	if _, _, ok := config.NormalizeEndpoint(address); !ok {
		return "", ErrSeedEndpoint
	}
	return address, nil
}

// DeriveAddress derives one node's address under a seed. It is NewDeriver
// followed by Address, for a caller with a single node.
func DeriveAddress(seed, node string) (string, error) {
	d, err := NewDeriver(seed)
	if err != nil {
		return "", err
	}
	return d.Address(node)
}
