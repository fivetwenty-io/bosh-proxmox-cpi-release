// Package lb provides a load-balancer registration interface and a concrete
// HAProxy Data Plane API (DPA) REST client implementation.
//
// The LBRegistrar interface allows CPI hooks to register or deregister VM
// instances as backend servers in a load balancer without coupling to a
// specific LB vendor. Registration failures are best-effort: callers should
// log and continue rather than fail the CPI action.
//
// HAProxyRegistrar implements LBRegistrar against the HAProxy DPA v3 runtime
// API (no reload required). It enforces TLS, redirect, and private-IP guards
// on the configured endpoint.
package lb
