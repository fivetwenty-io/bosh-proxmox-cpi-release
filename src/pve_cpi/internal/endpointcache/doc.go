// Package endpointcache holds the pieces of the API endpoint cache that do
// not depend on the endpoint pool. It validates the node names that discovery
// reads from /nodes, from a cache file, or from placement.az_map, and it
// derives the address a node answers on from a seed endpoint, refusing a seed
// whose domain, or whose derived host, is a public suffix. It builds the cache
// key, and the file and lock names, that bind a file to one endpoint list, one
// trust configuration, and one credential. It keeps the per-directory secret,
// creating it under a lock the first time a directory is used. It reads and
// writes the authenticated cache files through the private file store, which
// checks an HMAC and the file's key on every read and replaces a file
// atomically, under a lock, on every write. It chooses the cache directory,
// taking the directory the operator names when there is one and a per-user
// directory under the temporary directory otherwise. It also supplies the log
// level for the cache and discovery lines that the failure rules keep at Debug
// under the default mode and raise to Warn under an explicit "on". The package
// never contacts a PVE cluster.
package endpointcache
