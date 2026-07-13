package cryptstate

import (
	"bytes"
	"testing"
)

func newPair(t *testing.T) (client, server *CryptState) {
	t.Helper()
	server = &CryptState{}
	if err := server.GenerateKey(ModeOCB2AES128); err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	// Mirrors the client/server IV relationship in the real CryptSetup
	// handshake: server.EncryptIV becomes the client's DecryptIV and
	// vice versa.
	client = &CryptState{}
	if err := client.SetKey(ModeOCB2AES128, server.Key, append([]byte{}, server.DecryptIV...), append([]byte{}, server.EncryptIV...)); err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	return client, server
}

func TestCryptStateRoundTrip(t *testing.T) {
	client, server := newPair(t)

	plain := []byte("hello mumble")
	ct := make([]byte, len(plain)+client.Overhead())
	client.Encrypt(ct, plain)

	got := make([]byte, len(plain))
	if err := server.Decrypt(got, ct); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip mismatch: got %q, want %q", got, plain)
	}
	if server.Good != 1 {
		t.Fatalf("expected Good=1, got %d", server.Good)
	}
}

func TestCryptStateSequentialPackets(t *testing.T) {
	client, server := newPair(t)

	for i := 0; i < 300; i++ { // exceed 256 to exercise IV byte wraparound
		plain := []byte{byte(i), byte(i >> 8)}
		ct := make([]byte, len(plain)+client.Overhead())
		client.Encrypt(ct, plain)

		got := make([]byte, len(plain))
		if err := server.Decrypt(got, ct); err != nil {
			t.Fatalf("packet %d: Decrypt: %v", i, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("packet %d: mismatch: got %v, want %v", i, got, plain)
		}
	}
	if server.Good != 300 {
		t.Fatalf("expected Good=300, got %d", server.Good)
	}
	if server.Lost != 0 {
		t.Fatalf("expected Lost=0, got %d", server.Lost)
	}
}

func TestCryptStateDroppedPacketIsCounted(t *testing.T) {
	client, server := newPair(t)

	send := func(plain []byte) []byte {
		ct := make([]byte, len(plain)+client.Overhead())
		client.Encrypt(ct, plain)
		return ct
	}

	first := send([]byte("one"))
	_ = send([]byte("two")) // dropped, simulating a lost UDP datagram
	third := send([]byte("three"))

	got := make([]byte, 16)
	if err := server.Decrypt(got, first); err != nil {
		t.Fatalf("Decrypt(first): %v", err)
	}
	if err := server.Decrypt(got, third); err != nil {
		t.Fatalf("Decrypt(third): %v", err)
	}
	if server.Lost != 1 {
		t.Fatalf("expected Lost=1 after a dropped packet, got %d", server.Lost)
	}
}

func TestCryptStateTamperedCiphertextFailsAuth(t *testing.T) {
	client, server := newPair(t)

	plain := []byte("integrity check")
	ct := make([]byte, len(plain)+client.Overhead())
	client.Encrypt(ct, plain)
	ct[len(ct)-1] ^= 0xFF // flip a bit in the tag

	got := make([]byte, len(plain))
	if err := server.Decrypt(got, ct); err == nil {
		t.Fatal("expected tag mismatch error, got nil")
	}
}

func TestSetDecryptIVResync(t *testing.T) {
	client, server := newPair(t)

	// Simulate the client falling behind, then resyncing to the server's
	// current EncryptIV via a CryptSetup{server_nonce} message.
	if err := client.SetDecryptIV(server.EncryptIV); err != nil {
		t.Fatalf("SetDecryptIV: %v", err)
	}

	plain := []byte("resynced")
	ct := make([]byte, len(plain)+server.Overhead())
	server.Encrypt(ct, plain)

	got := make([]byte, len(plain))
	if err := client.Decrypt(got, ct); err != nil {
		t.Fatalf("Decrypt after resync: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip mismatch: got %q, want %q", got, plain)
	}
}
