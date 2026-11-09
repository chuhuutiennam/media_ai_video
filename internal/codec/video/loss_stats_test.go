package video

import (
	"testing"

	"github.com/pion/rtp"
)

// isIDRGroup: AU có slice IDR (Single NAL, FU-A hoặc STAP-A).
func isIDRGroup(g []*rtp.Packet) bool {
	for _, p := range g {
		if len(p.Payload) < 2 {
			continue
		}
		switch nt := p.Payload[0] & 0x1F; {
		case nt == 5:
			return true
		case nt == 28 && p.Payload[1]&0x1F == 5:
			return true
		case nt == 24:
			for i := 1; i+2 < len(p.Payload); {
				size := int(p.Payload[i])<<8 | int(p.Payload[i+1])
				if p.Payload[i+2]&0x1F == 5 {
					return true
				}
				i += 2 + size
			}
		}
	}
	return false
}

// Mất gói đầu của AU 12 (IDR mỗi 10 frame): decoder đếm 1 lần seq đứt, 1 đợt chờ IDR, bỏ các AU
// đã ghép xong tới IDR kế tiếp rồi hết chờ. Lần chờ IDR đầu stream không tính là một đợt.
func TestDecoder_PacketLossStats(t *testing.T) {
	requireFFmpeg(t)
	groups := genH264RTPFrameGroupsGOP(t, 320, 240, 10, 3, 10)
	const lostAU = 12
	nextIDR := -1
	for i := lostAU + 1; i < len(groups); i++ {
		if isIDRGroup(groups[i]) {
			nextIDR = i
			break
		}
	}
	if len(groups) != 30 || !isIDRGroup(groups[0]) || nextIDR != 20 {
		t.Fatalf("fixture: %d AU, IDR đầu=%v, IDR sau AU %d tại %d; muốn 30 AU, IDR 0/10/20", len(groups), isIDRGroup(groups[0]), lostAU, nextIDR)
	}

	dec, err := NewDecoder(Config{MaxWidth: 1920, MaxHeight: 1080}) // như pipeline
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if st := dec.Stats(); !st.WaitingForIDR || st.WaitIDREpisodes != 0 {
		t.Fatalf("đầu stream: muốn đang chờ IDR, 0 đợt; stats=%+v", st)
	}

	decoded := map[int]bool{}
	gapStatus := false
	for i, g := range groups {
		if i == lostAU {
			if st := dec.Stats(); st.PacketGaps != 0 || st.WaitIDREpisodes != 0 || st.WaitIDRDrops != 0 || st.WaitingForIDR {
				t.Fatalf("trước khi mất gói: stats=%+v", st)
			}
		}
		for j, p := range g {
			if i == lostAU && j == 0 {
				continue
			}
			status, err := dec.PushRTP(p.SequenceNumber, p.Timestamp, p.Marker, p.Payload)
			if err != nil {
				t.Fatalf("AU %d gói %d: %v", i, j, err)
			}
			if status == StatusPacketGap {
				gapStatus = true
			}
			for {
				if _, ok := dec.ReceiveFrame(); !ok {
					break
				}
				decoded[i] = true
			}
		}
		if i > lostAU && i < nextIDR && !dec.Stats().WaitingForIDR {
			t.Fatalf("AU %d: muốn đang chờ IDR", i)
		}
	}

	for i := range groups {
		want := i < lostAU || i >= nextIDR
		if decoded[i] != want {
			t.Errorf("AU %d: decoded=%v, muốn %v", i, decoded[i], want)
		}
	}
	st := dec.Stats()
	if !gapStatus || st.PacketGaps != 1 || st.WaitIDREpisodes != 1 || st.WaitIDRDrops != uint64(nextIDR-lostAU-1) || st.WaitingForIDR {
		t.Fatalf("gap_status=%v stats=%+v; muốn gaps=1 episodes=1 wait_drops=%d waiting=false", gapStatus, st, nextIDR-lostAU-1)
	}
}
