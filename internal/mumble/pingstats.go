package mumble

import "sync"

// pingStats keeps a small rolling window of round-trip-time samples (in
// milliseconds), reported as udp_ping_avg/udp_ping_var or
// tcp_ping_avg/tcp_ping_var in our outgoing Ping message. This is what
// populates the ping column/Statistics dialog a real Mumble client shows
// for other users — before this, we always sent zero-value stats, which is
// why a native client had nothing to display for this bridge's connection.
type pingStats struct {
	mu      sync.Mutex
	samples [12]float32
	n       int
	next    int
}

func (p *pingStats) add(ms float32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.samples[p.next] = ms
	p.next = (p.next + 1) % len(p.samples)
	if p.n < len(p.samples) {
		p.n++
	}
}

func (p *pingStats) avgVar() (avg, variance float32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n == 0 {
		return 0, 0
	}
	var sum float32
	for i := 0; i < p.n; i++ {
		sum += p.samples[i]
	}
	avg = sum / float32(p.n)
	var sqSum float32
	for i := 0; i < p.n; i++ {
		d := p.samples[i] - avg
		sqSum += d * d
	}
	return avg, sqSum / float32(p.n)
}
