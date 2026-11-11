// Package endpointpool holds the types and context markers that the failover
// transport for the Proxmox API shares with the rest of the CPI.
//
// A pool tries an ordered list of endpoints for one cluster. A request that
// fails before a connection exists provably never left the machine, so it may
// try the next endpoint. A request that may have been written is tethered to
// the endpoint it went to, so no layer above can replay it on another node.
// This package defines the vocabulary for that rule.
//
//   - ConnectPhaseError tags a failure that happened before any request byte
//     could be written, with the phase that failed. A final error that unwraps
//     to it proves the request was never sent.
//   - MaybeSentError marks an error from a request that may have reached the
//     endpoint.
//   - EntryUnreachableError reports a walk in which every hop failed in the
//     connect phase or on the header deadline.
//   - ErrSendByPassed is the refusal a request gets when its send-by deadline
//     has passed, and ErrTetherConflict is the refusal a mutation gets when its
//     tether key is conflicted for the rest of the call.
//   - WithSendBy, WithConfiguredOnly, and WithAffinityScope put the request
//     markers and the per-call scope on a context.
//   - Pool holds the ordered endpoints, the active endpoint and its
//     generation, the in-process demotions, and the transports it owns. Every
//     owned transport is a clone of the SDK's base transport that dials
//     through the pool's own tagging dialer and speaks only HTTP/1.1, and a
//     discovered endpoint's clones accept only a certificate its pin set, or
//     under verify_ssl true its name, vouches for.
//   - A walk tries the configured endpoints first, then the discovered ones
//     with the nodes a quorate cluster status reported online first and each
//     group in node-name order, and it moves an endpoint that is demoted or
//     carries a down mark younger than two minutes to the end. A discovered
//     endpoint that fails its pin is left out for the rest of the process,
//     and DropDiscovered leaves every discovered endpoint and every cold-start
//     candidate out the same way when discovery finds that the cluster
//     identity changed. The pool reports down marks to a MarkSink and the
//     first answer from a configured endpoint to a FirstAnswerHook, two
//     interfaces the endpoint cache implements, so this package never imports
//     the cache. It calls both on the request's goroutine, so each
//     implementation hands its work to a goroutine of its own and returns at
//     once.
//   - A pool may carry cold-start candidates, the nodes the entry's
//     placement.az_map names, when Gate says the cache holds no usable
//     discovered entry. A walk reaches them only after every configured
//     endpoint failed its dial, DNS lookup, or handshake in that walk, and
//     never for the login, a configured-only or routed request, or a
//     mutation tethered to a configured endpoint, and the same holds for the
//     candidate a probe accepted. A mutation tethered to the accepted
//     candidate itself follows its tether there until the candidate fails
//     TLS verification, and is refused unsent after that. The process runs
//     one cold start, which probes each candidate that answers once at most
//     through a Prober the caller supplies, and a candidate carries a request
//     only after its certificate chains to the operator's CA, covers its name,
//     and its probe proves its membership. A probe the request's own context
//     cut before the candidate sent any byte of a response refuses nothing,
//     and the next request that reaches the cold start probes that candidate.
//   - A pool's tether table ties each mutation that may have been sent to the
//     endpoint it went to, by a tether key of its method and path, or of its
//     family for the HA, replication, and pool writers. The first mutation
//     with a key reserves the tether before any byte can leave, every later
//     one follows it, and a call's tethers, along with the ones made without
//     a scope, are dropped when the call ends.
//
// The package imports the standard library, the CPI's logger in internal/log,
// and the SDK's client package, which it uses only for the redirect sentinel
// and the permanent marker a redirect refusal carries. It must never import
// internal/pve, internal/endpointcache, or the handlers, so that every one of
// those packages can import it.
package endpointpool
