package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeferredSourcePNGPreservesRawTimelineAndStandaloneExport(t *testing.T) {
	frames, err := h264Frames(context.Background(), 64, 48, 3, time.Second, "", 88001)
	if err != nil {
		t.Fatal(err)
	}
	var packets []timedPacket
	for _, frame := range frames {
		for _, packet := range frame {
			packets = append(packets, timedPacket{Packet: packet})
		}
	}
	for _, debugPNG := range []bool{false, true} {
		root := t.TempDir()
		if err := dumpFramesWithOptions(root, packets, debugPNG); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(root, "frames.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest []sourceFrame
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		if len(manifest) != 3 {
			t.Fatalf("frames=%d", len(manifest))
		}
		raw, err := os.Stat(filepath.Join(root, "frames.rgba"))
		if err != nil {
			t.Fatal(err)
		}
		if raw.Size() != int64(len(manifest)*64*48*4) {
			t.Fatalf("raw size=%d", raw.Size())
		}
		for i, f := range manifest {
			if f.RGBAOffset != uint64(i*64*48*4) {
				t.Fatalf("offset=%d", f.RGBAOffset)
			}
			_, err := os.Stat(filepath.Join(root, f.File))
			if debugPNG && err != nil {
				t.Fatal(err)
			}
			if !debugPNG && !os.IsNotExist(err) {
				t.Fatal("debug source PNG was encoded before playout")
			}
		}
	}
}
