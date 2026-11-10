// Package endpointcache holds the pieces of the API endpoint cache that do
// not depend on the endpoint pool. It validates the node names that discovery
// reads from /nodes, from a cache file, or from placement.az_map, and it
// derives the address a node answers on from a seed endpoint, refusing a seed
// whose domain is a public suffix. It builds the cache key and file name that
// bind a file to one endpoint list, one trust configuration, and one
// credential. It keeps the per-directory secret and reads and writes the
// authenticated cache files through the private file store. It also locates
// the cache directory and sweeps stale files out of it. The package never
// contacts a PVE cluster.
package endpointcache
