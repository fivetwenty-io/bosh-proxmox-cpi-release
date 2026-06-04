package lb

import "context"

// Server describes a backend server to be registered in a load balancer.
type Server struct {
	// Name is the server label used by the LB (e.g. "vm-100").
	Name string
	// Address is the guest IP address.
	Address string
	// Port is the backend TCP port.
	Port int
}

// Registrar is the interface for registering and deregistering VM instances
// as backend servers in a load balancer.
//
// Implementations must be safe for concurrent use. Registration and
// deregistration are best-effort: callers should log errors and continue
// rather than propagating them as hard CPI failures.
type Registrar interface {
	// Register adds s as a server in backend. Idempotent: adding an
	// already-present server must succeed.
	Register(ctx context.Context, backend string, s Server) error

	// Deregister removes the server identified by serverName from backend.
	// Idempotent: removing an absent server must succeed.
	Deregister(ctx context.Context, backend, serverName string) error
}
