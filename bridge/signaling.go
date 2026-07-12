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

type userListMsg struct {
	Type  string   `json:"type"`
	Users []string `json:"users"`
}

type userEventMsg struct {
	Type     string `json:"type"`
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
