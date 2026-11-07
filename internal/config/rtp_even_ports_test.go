package config

import (
	"strings"
	"testing"
)

func TestRTPEvenPorts(t *testing.T) {
	cfg, err := Load(writeTemp(t, "rtp:\n  port_start: 40000\n  port_end: 40099\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RTP.EvenPorts || !cfg.ToControlPlaneServerConfig().RTPEvenPorts {
		t.Fatal("rtp.even_ports must default to true and reach the control plane")
	}

	_, err = Load(writeTemp(t, "rtp:\n  port_start: 40001\n  port_end: 40099\n"))
	if err == nil || !strings.Contains(err.Error(), "rtp.port_start must be even") {
		t.Fatalf("odd port_start with even_ports: want error, got %v", err)
	}

	cfg, err = Load(writeTemp(t, "rtp:\n  port_start: 40001\n  port_end: 40099\n  even_ports: false\n"))
	if err != nil || cfg.ToControlPlaneServerConfig().RTPEvenPorts {
		t.Fatalf("even_ports=false must allow odd port_start: %v", err)
	}
}
