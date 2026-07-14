package mumble

import (
	"errors"

	"github.com/golang/protobuf/proto"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/MumbleProto"
)

func (c *Client) handleReject(data []byte) error {
	var p MumbleProto.Reject
	if err := proto.Unmarshal(data, &p); err != nil {
		return err
	}
	reason := p.GetReason()
	if reason == "" {
		reason = p.GetType().String()
	}
	c.completeHandshake(errors.New("mumble: connection rejected: " + reason))
	return nil
}

func (c *Client) handleServerSync(data []byte) error {
	var p MumbleProto.ServerSync
	if err := proto.Unmarshal(data, &p); err != nil {
		return err
	}

	c.mu.Lock()
	if p.Session != nil {
		c.session = *p.Session
		c.self = c.users[c.session]
	}
	c.synced = true
	c.mu.Unlock()

	c.completeHandshake(nil)
	if c.cfg.OnConnect != nil {
		c.cfg.OnConnect(c, p.GetWelcomeText())
	}
	return nil
}

func (c *Client) handleChannelState(data []byte) error {
	var p MumbleProto.ChannelState
	if err := proto.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.ChannelId == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	ch := c.channels[*p.ChannelId]
	if ch == nil {
		ch = newChannel(*p.ChannelId)
		c.channels[*p.ChannelId] = ch
	}
	if p.Name != nil {
		ch.Name = *p.Name
	}
	if p.Parent != nil {
		parent := c.channels[*p.Parent]
		if parent == nil {
			parent = newChannel(*p.Parent)
			c.channels[*p.Parent] = parent
		}
		if ch.Parent != parent {
			if ch.Parent != nil {
				delete(ch.Parent.Children, ch.ID)
			}
			ch.Parent = parent
			parent.Children[ch.ID] = ch
		}
	}
	return nil
}

func (c *Client) handleChannelRemove(data []byte) error {
	var p MumbleProto.ChannelRemove
	if err := proto.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.ChannelId == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	ch := c.channels[*p.ChannelId]
	if ch == nil {
		return nil
	}
	if ch.Parent != nil {
		delete(ch.Parent.Children, ch.ID)
	}
	delete(c.channels, *p.ChannelId)
	return nil
}

func (c *Client) handleUserState(data []byte) error {
	var p MumbleProto.UserState
	if err := proto.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.Session == nil {
		return nil
	}
	session := *p.Session

	c.mu.Lock()

	u := c.users[session]
	isNew := u == nil
	if u == nil {
		u = &User{Session: session}
		c.users[session] = u
	}
	oldMuted, oldSelfMuted := u.Muted, u.SelfMuted
	oldDeafened, oldSelfDeafened := u.Deafened, u.SelfDeafened

	if p.Name != nil {
		u.Name = *p.Name
	}
	if p.SelfMute != nil {
		u.SelfMuted = *p.SelfMute
	}
	if p.Mute != nil {
		u.Muted = *p.Mute
	}
	if p.SelfDeaf != nil {
		u.SelfDeafened = *p.SelfDeaf
	}
	if p.Deaf != nil {
		u.Deafened = *p.Deaf
	}
	muteChanged := u.Muted != oldMuted || u.SelfMuted != oldSelfMuted
	deafChanged := u.Deafened != oldDeafened || u.SelfDeafened != oldSelfDeafened

	channelChanged := false
	if p.ChannelId != nil || u.Channel == nil {
		targetID := uint32(0)
		if p.ChannelId != nil {
			targetID = *p.ChannelId
		}
		newCh := c.channels[targetID]
		if newCh == nil {
			newCh = c.channels[0]
		}
		if newCh != nil && newCh != u.Channel {
			if u.Channel != nil {
				delete(u.Channel.Users, session)
			}
			u.Channel = newCh
			newCh.Users[session] = u
			channelChanged = true
		}
	}
	if session == c.session {
		c.self = u
	}
	synced := c.synced
	name := u.Name
	muted, selfMuted := u.Muted, u.SelfMuted
	deafened, selfDeafened := u.Deafened, u.SelfDeafened

	c.mu.Unlock()

	if !synced {
		// Initial roster population during the handshake; not a live event.
		return nil
	}
	if isNew && c.cfg.OnUserJoined != nil {
		c.cfg.OnUserJoined(c, name)
	} else if channelChanged && c.cfg.OnUserMoved != nil {
		c.cfg.OnUserMoved(c)
	}
	if !isNew {
		if muteChanged && c.cfg.OnUserMuteChanged != nil {
			c.cfg.OnUserMuteChanged(c, name, muted, selfMuted)
		}
		if deafChanged && c.cfg.OnUserDeafChanged != nil {
			c.cfg.OnUserDeafChanged(c, name, deafened, selfDeafened)
		}
	}
	return nil
}

func (c *Client) handleUserRemove(data []byte) error {
	var p MumbleProto.UserRemove
	if err := proto.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.Session == nil {
		return nil
	}

	c.mu.Lock()
	u := c.users[*p.Session]
	if u == nil {
		c.mu.Unlock()
		return nil
	}
	delete(c.users, *p.Session)
	if u.Channel != nil {
		delete(u.Channel.Users, *p.Session)
	}
	synced := c.synced
	c.mu.Unlock()

	if synced && c.cfg.OnUserLeft != nil {
		c.cfg.OnUserLeft(c, u.Name)
	}
	return nil
}

func (c *Client) handleTextMessage(data []byte) error {
	var p MumbleProto.TextMessage
	if err := proto.Unmarshal(data, &p); err != nil {
		return err
	}

	from := ""
	if p.Actor != nil {
		c.mu.Lock()
		if u := c.users[*p.Actor]; u != nil {
			from = u.Name
		}
		c.mu.Unlock()
	}
	if c.cfg.OnTextMessage != nil {
		c.cfg.OnTextMessage(c, from, p.GetMessage())
	}
	return nil
}
