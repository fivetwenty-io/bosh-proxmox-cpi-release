package pvetest

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

// DNS header and question sizes, and the codes the stub name server writes.
const (
	dnsHeaderLen   = 12
	dnsQuestionEnd = 4
	dnsTypeA       = 1
	dnsClassIN     = 1
	dnsRcodeName   = 3
	dnsTTLSeconds  = 60
	dnsMaxPacket   = 512
)

// InstallNameServer makes every host name resolve to 127.0.0.1 for the rest of
// the test, except the names refused reports as missing, which answer
// NXDOMAIN. It replaces net.DefaultResolver with one that asks a stub name
// server on a loopback UDP port, and restores the resolver when the test ends.
// refused may be nil. It receives the queried name in lower case without the
// trailing dot, and the resolver may query the name with the system's search
// domains appended, so refuse by prefix or suffix and not by equality.
//
// The discovery worker checks the names it derives with the system resolver,
// and a dial to a derived name needs one too, so a test that runs either
// through a production client has no other way to make pve2.dc1.example.com
// resolve. The resolver is process-wide, so a test that installs it must not
// run in parallel with another test that resolves a name.
func InstallNameServer(tb testing.TB, refused func(name string) bool) {
	tb.Helper()
	pc, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("pvetest: listen for the stub name server: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, dnsMaxPacket)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := nameReply(buf[:n], refused); reply != nil {
				_, _ = pc.WriteTo(reply, addr)
			}
		}
	}()
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
		},
	}
	tb.Cleanup(func() {
		net.DefaultResolver = previous
		_ = pc.Close()
		<-done
	})
}

// nameReply answers one query packet, or returns nil for a packet that is not a
// single-question query it understands.
func nameReply(query []byte, refused func(string) bool) []byte {
	if len(query) < dnsHeaderLen {
		return nil
	}
	var labels []string
	end := dnsHeaderLen
	for end < len(query) && query[end] != 0 {
		size := int(query[end])
		if size > 63 || end+1+size > len(query) {
			return nil
		}
		labels = append(labels, string(query[end+1:end+1+size]))
		end += size + 1
	}
	questionEnd := end + 1 + dnsQuestionEnd
	if end >= len(query) || questionEnd > len(query) {
		return nil
	}
	name := strings.ToLower(strings.Join(labels, "."))
	reply := append([]byte(nil), query[:questionEnd]...)
	// Response, recursion available, no error; no answers unless set below.
	reply[2], reply[3] = 0x81, 0x80
	for i := 6; i < dnsHeaderLen; i++ {
		reply[i] = 0
	}
	if refused != nil && refused(name) {
		reply[3] |= dnsRcodeName
		return reply
	}
	if binary.BigEndian.Uint16(query[end+1:]) == dnsTypeA {
		reply[7] = 1
		reply = append(reply, 0xc0, dnsHeaderLen, 0, dnsTypeA, 0, dnsClassIN)
		reply = binary.BigEndian.AppendUint32(reply, dnsTTLSeconds)
		reply = append(reply, 0, 4, 127, 0, 0, 1)
	}
	return reply
}
