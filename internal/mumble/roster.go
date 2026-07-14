package mumble

// Channel is a node in the server's channel tree.
type Channel struct {
	ID       uint32
	Name     string
	Parent   *Channel
	Children map[uint32]*Channel
	Users    map[uint32]*User
}

func newChannel(id uint32) *Channel {
	return &Channel{
		ID:       id,
		Children: make(map[uint32]*Channel),
		Users:    make(map[uint32]*User),
	}
}

// find returns the channel whose path (by channel name) from c matches
// names, or nil if there is no such channel. For example, given:
//
//	Root
//	  Games
//	    Sub
//
// root.find([]string{"Games", "Sub"}) returns the "Sub" channel.
func (c *Channel) find(names []string) *Channel {
	if len(names) == 0 {
		return c
	}
	for _, child := range c.Children {
		if child.Name == names[0] {
			return child.find(names[1:])
		}
	}
	return nil
}

// User is a user currently connected to the server.
type User struct {
	Session      uint32
	Name         string
	Channel      *Channel
	Muted        bool
	SelfMuted    bool
	Deafened     bool
	SelfDeafened bool
}

// UserStatus is a snapshot of a User's display-relevant state, safe to hand
// to callers outside the Client's lock (unlike *User, it holds no pointer
// into the live roster/channel tree).
type UserStatus struct {
	Name         string
	Muted        bool
	SelfMuted    bool
	Deafened     bool
	SelfDeafened bool
}
