package client

// clientAtVersion is a client literal with a protocol version in effect, for
// tests of the paths that branch on the era without a connection.
func clientAtVersion(version string) *Client {
	c := &Client{}
	c.era.Store(&era{version: version})
	return c
}
