package mumble

// Config holds the credentials and event callbacks for a Client. Unlike
// gumble's generic multi-listener event bus, this client only ever needs a
// single subscriber (one bridge Peer per Client), so callbacks are plain
// fields.
//
// Callbacks fire on the Client's internal read goroutine — including,
// crucially, possibly before Dial has returned to its caller (the handshake
// completes and OnConnect fires from inside that same goroutine, racing
// with Dial's caller storing the returned *Client anywhere). Every callback
// is therefore passed the Client explicitly so implementations never need
// to depend on a Dial call having already returned; use the passed-in c
// rather than a captured reference to the Dial() result. Callbacks must not
// block or re-enter the Client synchronously in a way that could deadlock,
// and should treat the passed-in strings/bytes as read-only.
type Config struct {
	Username string
	Password string
	Tokens   []string

	// DisableUDP keeps the connection on the TCP tunnel even after a
	// working UDP path is confirmed. Meant as a bisection tool while
	// diagnosing transport-specific audio issues, and as an escape hatch
	// for networks that block/mangle UDP to the Mumble server.
	DisableUDP bool

	// OnConnect fires once the initial connection handshake completes
	// (channel tree and user roster populated, self identified).
	OnConnect func(c *Client, welcomeMessage string)
	// OnDisconnect fires when the connection is lost or closed, after
	// OnConnect has fired. err is nil for a locally-initiated Disconnect.
	OnDisconnect func(c *Client, err error)
	// OnTextMessage fires for chat messages. from is "" for server-generated
	// messages.
	OnTextMessage func(c *Client, from, message string)
	// OnUserJoined/OnUserLeft fire when another user's presence on the
	// server changes. Not fired for the client's own connect/disconnect.
	OnUserJoined func(c *Client, name string)
	OnUserLeft   func(c *Client, name string)
	// OnUserMoved fires whenever any user's channel membership changes
	// (including the client's own). Callers that care about "who's in my
	// channel" should re-read Client.SelfChannelUsers.
	OnUserMoved func(c *Client)
	// OnAudio fires once per received audio packet, carrying the raw
	// (undecoded) Opus payload. session identifies the speaking user.
	OnAudio func(c *Client, session uint32, seq int64, final bool, opus []byte)
}
