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
// treats as one, so anyone could register the node's name under it. The
// derivation then yields no address. ErrSeedEndpoint means the seed does not
// meet the endpoint grammar, or the derived name would not be a legal host.
// Neither error echoes the seed.
var (
	ErrPublicSuffix = errors.New("seed domain is a public suffix")
	ErrSeedEndpoint = errors.New("seed is not a valid endpoint to derive node addresses from")
)

// Deriver turns node names into endpoint addresses under one seed. It holds
// the seed's domain, which is empty for a bare-name or IP seed, and its port.
// A Deriver is immutable and safe for concurrent use.
type Deriver struct {
	domain string
	port   int
}

// NewDeriver prepares address derivation for a seed written in the endpoints
// list grammar. When the seed is a fully qualified name, meaning it holds a dot
// and is not an IP literal, nodes take the seed's domain, which is everything
// after its first label. The domain must pass publicsuffix.EffectiveTLDPlusOne,
// and otherwise the error wraps ErrPublicSuffix. When the seed is a bare name
// or an IP literal, nodes keep their bare name. Every address uses the seed's
// port, which is 8006 when the seed has none.
func NewDeriver(seed string) (*Deriver, error) {
	host, port, ok := config.NormalizeEndpoint(seed)
	if !ok {
		return nil, ErrSeedEndpoint
	}
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return &Deriver{port: port}, nil
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" {
			return nil, ErrSeedEndpoint
		}
	}
	_, domain, _ := strings.Cut(host, ".")
	if _, err := publicsuffix.EffectiveTLDPlusOne(domain); err != nil {
		return nil, ErrPublicSuffix
	}
	return &Deriver{domain: domain, port: port}, nil
}

// Address returns the host:port a node answers on. The node name must pass
// ValidateNodeName, and a failure wraps its tier error so the caller can tell
// a dropped node from a rejected source. The result meets the endpoints list
// grammar, and its host is at most 253 characters.
func (d *Deriver) Address(node string) (string, error) {
	if err := ValidateNodeName(node); err != nil {
		return "", err
	}
	host := node
	if d.domain != "" {
		host = node + "." + d.domain
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
