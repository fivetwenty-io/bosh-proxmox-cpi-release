//go:build !unix

package spreadclaim

// processAlive reports every process as running where the CPI cannot ask, so
// a claim lapses only at its expiry.
func processAlive(int) bool { return true }
