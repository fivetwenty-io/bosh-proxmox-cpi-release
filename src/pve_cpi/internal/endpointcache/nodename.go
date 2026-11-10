package endpointcache

import (
	"errors"
	"strings"
)

// MaxNodeNameLength is the longest node name the helper accepts. It is the
// limit on one DNS label.
const MaxNodeNameLength = 63

// Node name refusals. ErrNodeNameFormat is the first tier, a name that breaks
// PVE's pve-node grammar, and a caller treats it as proof that the whole
// source is untrustworthy. ErrNodeNameAmbiguous is the second tier, a name the
// grammar allows but that a resolver could read as an address we never meant,
// and a caller drops that one node and keeps the rest. A refusal wraps exactly
// one of them, and the error text never echoes the name.
var (
	ErrNodeNameFormat    = errors.New("node name is not a valid pve-node label")
	ErrNodeNameAmbiguous = errors.New("node name could resolve as a numeric or loopback address")
)

// ValidateNodeName checks a node name read from /nodes, from a cache file, or
// from placement.az_map. A name passes when it is one DNS label of ASCII
// letters, digits, and inner hyphens, at most MaxNodeNameLength characters,
// and it is not a name a resolver could turn into an address. The refusals
// are a name made only of digits, a name that starts with 0x or 0X and
// continues only in hex digits (including 0x alone), and localhost,
// ip6-localhost, and ip6-loopback in any case. The grammar is the one
// validStorageIdentityNode applies, with the length cap added.
func ValidateNodeName(name string) error {
	if !nodeNameFormat(name) {
		return ErrNodeNameFormat
	}
	if allDigits(name) || hexSpelling(name) || loopbackAlias(name) {
		return ErrNodeNameAmbiguous
	}
	return nil
}

// loopbackAliases are the names Debian's stock /etc/hosts maps to a loopback
// address.
var loopbackAliases = []string{"localhost", "ip6-localhost", "ip6-loopback"}

// loopbackAlias reports whether name is one of loopbackAliases in any case.
func loopbackAlias(name string) bool {
	for _, alias := range loopbackAliases {
		if strings.EqualFold(name, alias) {
			return true
		}
	}
	return false
}

// ValidNodeName reports whether ValidateNodeName accepts name.
func ValidNodeName(name string) bool {
	return ValidateNodeName(name) == nil
}

// nodeNameFormat applies PVE's pve-node grammar: alphanumeric at both ends,
// alphanumerics and hyphens between, at least one character, and no more than
// MaxNodeNameLength. It reads bytes, so a multi-byte character fails.
func nodeNameFormat(name string) bool {
	if name == "" || len(name) > MaxNodeNameLength {
		return false
	}
	if !asciiAlphaNumeric(name[0]) || !asciiAlphaNumeric(name[len(name)-1]) {
		return false
	}
	for i := 1; i < len(name)-1; i++ {
		if !asciiAlphaNumeric(name[i]) && name[i] != '-' {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// allDigits reports whether name is non-empty and made only of ASCII digits.
func allDigits(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

// hexSpelling reports whether name is 0x or 0X followed by nothing but hex
// digits, the form inet_aton reads as a number.
func hexSpelling(name string) bool {
	if len(name) < 2 || name[0] != '0' || (name[1] != 'x' && name[1] != 'X') {
		return false
	}
	for i := 2; i < len(name); i++ {
		c := name[i]
		isHex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if !isHex {
			return false
		}
	}
	return true
}
