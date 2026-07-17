package bridge

import "encoding/json"

type msgEnvelope struct {
	Type string `json:"type"`
}

type loginMsg struct {
	Type     string `json:"type"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type sdpMsg struct {
	Type    string `json:"type"`
	SDPType string `json:"sdpType"`
	SDP     string `json:"sdp"`
}

type iceMsg struct {
	Type          string `json:"type"`
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdpMid"`
	SDPMLineIndex uint16 `json:"sdpMLineIndex"`
}

type textMsg struct {
	Type    string `json:"type"`
	From    string `json:"from,omitempty"`
	Message string `json:"message"`
}

type muteMsg struct {
	Type  string `json:"type"`
	Muted bool   `json:"muted"`
}

type deafMsg struct {
	Type     string `json:"type"`
	Deafened bool   `json:"deafened"`
}

type userInfo struct {
	Name         string `json:"name"`
	Muted        bool   `json:"muted"`
	SelfMuted    bool   `json:"selfMuted"`
	Deafened     bool   `json:"deafened"`
	SelfDeafened bool   `json:"selfDeafened"`
}

type userListMsg struct {
	Type  string     `json:"type"`
	Users []userInfo `json:"users"`
}

type userEventMsg struct {
	Type     string `json:"type"`
	Username string `json:"username"`
}

// muteStateMsg and deafStateMsg are separate message types (rather than one
// combined userStateMsg) because each is only sent when that particular pair
// of flags actually changed — combining them would force sending a
// zero-valued guess for whichever pair didn't change, and JSON can't
// distinguish a false value from "unchanged, don't touch this field".
type muteStateMsg struct {
	Type      string `json:"type"`
	Username  string `json:"username"`
	Muted     bool   `json:"muted"`
	SelfMuted bool   `json:"selfMuted"`
}

type deafStateMsg struct {
	Type         string `json:"type"`
	Username     string `json:"username"`
	Deafened     bool   `json:"deafened"`
	SelfDeafened bool   `json:"selfDeafened"`
}

type talkingMsg struct {
	Type     string `json:"type"`
	Username string `json:"username"`
	Talking  bool   `json:"talking"`
}

// roomEventMsg is a synthetic, browser-only chat-log line for a roster
// change (join/leave/mute/deafen) — never a real Mumble TextMessage.
// Kind is one of: joined, left, muted, unmuted, deafened, undeafened.
type roomEventMsg struct {
	Type     string `json:"type"`
	Kind     string `json:"kind"`
	Username string `json:"username"`
}

type errorMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type connectedMsg struct {
	Type string `json:"type"`
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
