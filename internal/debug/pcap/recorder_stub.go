//go:build !linux

package pcap

import "errors"

// Recorder is a no-op stub on non-Linux platforms where AF_PACKET is unavailable.
type Recorder struct{}

// New always returns an error on non-Linux platforms.
func New(_ Config) (*Recorder, error) {
	return nil, errors.New("pcap: AF_PACKET capture is only supported on Linux")
}

func (r *Recorder) Start()        {}
func (r *Recorder) Stop()         {}
func (r *Recorder) Stats() Stats  { return Stats{} }
