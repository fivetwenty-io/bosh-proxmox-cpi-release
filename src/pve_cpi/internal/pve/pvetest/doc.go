// Package pvetest holds the TLS test servers that the wire tests of internal/pve and
// internal/pve/endpointpool share.
//
// A test certificate authority signs any number of leaf certificates, and each
// server kind reproduces one way a Proxmox endpoint fails, so the failover tests
// can drive a real net/http transport against it:
//
//   - Live answers fake PVE routes and records every request.
//   - Reset holds a loopback port and resets each connection after one byte, so a
//     TLS handshake fails with a connection reset and no request exists.
//   - Refused offers a DialContext that fails like a dial to a closed port, with
//     an ECONNREFUSED dial error, and owns a held address for routes to name.
//   - Dropped accepts TCP and never answers, and offers a DialContext that blocks
//     until its context ends, so a dial or a handshake times out.
//   - Garbage writes bytes that are not a TLS record when a client connects.
//   - Hung completes the TLS handshake, reads a request, and never answers.
//   - BodyThenHijack reads a whole request body, then hijacks the connection and
//     closes it, so a mutation may have reached the server but gets no answer.
//   - ReuseCloser answers the first request on each connection, then closes the
//     connection when a later request arrives on it, before reading that request.
//
// Every server counts connections, handshakes, and requests per method with
// atomic counters, keeps the requests it read, and announces connections,
// requests, dials, and reused closes on channels, so a test waits on an event
// where it would otherwise sleep.
//
// The package imports testing, so only _test.go files may import it. A guard
// test in this package fails when a production file does.
package pvetest
