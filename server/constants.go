package server

// Common HTTP header constants used across server transports
const (
	HeaderKeySessionID       = "Mcp-Session-Id"
	HeaderKeyProtocolVersion = "Mcp-Protocol-Version"
)

// headerKeyLastEventID is [mcp.HeaderLastEventID] in the canonical form
// net/http stores incoming header keys in, so a lookup by it needs no
// conversion.
const headerKeyLastEventID = "Last-Event-Id"
