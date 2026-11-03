package rtpout

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// EgressPacer paces outbound RTP packets at fixed ptime intervals so MF
// receives one frame per tick (real-time) rather than a burst when AI returns
// a full utterance at once.
//
// Push is non-blocking and uses all-or-nothing drop: if the queue lacks
// capacity for the entire payload, no frames are enqueued (prevents timestamp
// compression from mid-utterance gaps). Multiple consecutive utterances are
// concatenated — each Push appends frames to the same queue.
//
// Marker bit semantics: first frame ever → Marker=true; after a silence gap
// (≥1 empty tick) → Marker=true on the next sent frame. Silence gaps also
// advance the RTP timestamp so MF's jitter buffer correctly spaces playback.
type EgressPacer struct {
	sender    *Sender
	frameQ    chan []byte
	frameSize int
	ptimeMs   int
	sessionID string
}

// NewPacer creates an EgressPacer.
//   - sender:      underlying RTP sender (reuses per-session UDP socket).
//   - sessionID:   used in log fields for observability.
//   - ptimeMs:     packet time in ms (typically 20); sets ticker interval.
//   - queueFrames: max buffered frames; Push drops entire payload when full.
func NewPacer(sender *Sender, sessionID string, ptimeMs, queueFrames int) *EgressPacer {
	return &EgressPacer{
		sender:    sender,
		frameQ:    make(chan []byte, queueFrames),
		frameSize: int(sender.tsIncr),
		ptimeMs:   ptimeMs,
		sessionID: sessionID,
	}
}

// Push slices payload into ptimeMs frames and enqueues them for paced delivery.
// Partial remainder is padded to frameSize with 0xFF (µ-law silence).
// All-or-nothing: returns totalFrames and enqueues nothing if queue lacks capacity.
func (p *EgressPacer) Push(payload []byte) int {
	if p.frameSize <= 0 {
		return 0
	}
	fullFrames := len(payload) / p.frameSize
	remainder := len(payload) % p.frameSize
	totalFrames := fullFrames
	if remainder > 0 {
		totalFrames++
	}
	if totalFrames == 0 {
		return 0
	}
	// All-or-nothing: reject entire payload if capacity is insufficient.
	if len(p.frameQ)+totalFrames > cap(p.frameQ) {
		return totalFrames
	}
	for i := 0; i < fullFrames; i++ {
		frame := make([]byte, p.frameSize)
		copy(frame, payload[i*p.frameSize:(i+1)*p.frameSize])
		p.frameQ <- frame
	}
	if remainder > 0 {
		frame := make([]byte, p.frameSize)
		for i := range frame {
			frame[i] = 0xFF // µ-law silence
		}
		copy(frame, payload[fullFrames*p.frameSize:])
		p.frameQ <- frame
	}
	return 0
}

// Run drives the pacing loop; must be called in a dedicated goroutine.
// On each tick, one queued frame is sent; empty ticks are silent (no silence
// insertion — the gap shows up as a timestamp advance + Marker on next frame).
// Exits when ctx is cancelled.
func (p *EgressPacer) Run(ctx context.Context) {
	interval := time.Duration(p.ptimeMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var started bool
	var lastSendTick time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-ticker.C:
			select {
			case frame := <-p.frameQ:
				marker := !started
				if started {
					// Count how many ticks elapsed since last send; advance ts for gaps.
					slots := int(tick.Sub(lastSendTick) / interval)
					if slots < 1 {
						slots = 1
					}
					if missingFrames := slots - 1; missingFrames > 0 {
						p.sender.AdvanceFrames(missingFrames)
						marker = true
					}
				}
				var err error
				if marker {
					err = p.sender.SendMarked(frame)
				} else {
					err = p.sender.Send(frame)
				}
				if err != nil {
					zap.L().Debug("rtp: egress send failed", zap.String("session_id", p.sessionID), zap.Error(err))
				}
				started = true
				lastSendTick = tick
			default:
			}
		}
	}
}

// Queued returns the number of frames currently buffered (diagnostic use).
func (p *EgressPacer) Queued() int { return len(p.frameQ) }
