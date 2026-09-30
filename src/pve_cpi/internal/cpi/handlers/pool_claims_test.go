package handlers_test

import "sync"

// lockClaims remembers the comment each create stamped on a pool, the way PVE
// does. A cluster lock reads its claim back after every create, so a fake whose
// create succeeds has to answer that read with the claim it was given.
type lockClaims struct {
	mu     sync.Mutex
	claims map[string]string
}

func (c *lockClaims) put(pool, comment string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claims == nil {
		c.claims = map[string]string{}
	}
	c.claims[pool] = comment
}

func (c *lockClaims) drop(pool string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.claims, pool)
}

func (c *lockClaims) get(pool string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	claim, found := c.claims[pool]
	return claim, found, nil
}
