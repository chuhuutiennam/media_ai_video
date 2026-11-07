package controlplane

import (
	"net/http"
	"testing"
	"time"
)

// Cổng trống là hàng đợi FIFO: cổng vừa trả xếp cuối, không được cấp lại ngay cho cuộc gọi kế tiếp.
func TestPortAllocator_FIFO(t *testing.T) {
	a, _ := NewPortAllocator(40000, 40003)
	for _, want := range []int{40000, 40001, 40002, 40003} {
		if p, err := a.Acquire(); err != nil || p != want {
			t.Fatalf("acquire: got %d, %v; want %d", p, err, want)
		}
	}
	a.Release(40001)
	a.Release(40000)
	for _, want := range []int{40001, 40000} {
		if p, _ := a.Acquire(); p != want {
			t.Fatalf("release order not kept: got %d, want %d", p, want)
		}
	}

	b, _ := NewPortAllocator(40000, 40003)
	p, _ := b.Acquire()
	b.Release(p)
	if next, _ := b.Acquire(); next == p {
		t.Fatalf("just-released port %d handed out again while others are free", p)
	}
}

func TestPortAllocator_ReuseDelay(t *testing.T) {
	clock := time.Unix(1000, 0)
	a, _ := NewEvenPortAllocator(40000, 40005) // 40000, 40002, 40004
	a.now = func() time.Time { return clock }
	a.SetReuseDelay(5 * time.Second)
	for range 3 {
		if _, err := a.Acquire(); err != nil {
			t.Fatal(err)
		}
	}
	a.Release(40002)
	clock = clock.Add(time.Second)
	a.Release(40000)
	if a.Available() != 0 || a.Cooling() != 2 || a.InUse() != 1 {
		t.Fatalf("cooling ports must not count as free: available=%d cooling=%d inUse=%d", a.Available(), a.Cooling(), a.InUse())
	}
	if _, err := a.Acquire(); err != ErrNoPortsAvailable {
		t.Fatalf("all free ports cooling: want ErrNoPortsAvailable, got %v", err)
	}

	clock = clock.Add(4 * time.Second) // 40002 vừa hết hạn, 40000 còn 1s
	if a.Available() != 1 || a.Cooling() != 1 {
		t.Fatalf("after 5s: available=%d cooling=%d", a.Available(), a.Cooling())
	}
	if p, err := a.Acquire(); err != nil || p != 40002 {
		t.Fatalf("want 40002 first, got %d, %v", p, err)
	}
	if _, err := a.Acquire(); err != ErrNoPortsAvailable {
		t.Fatalf("40000 still cooling: got %v", err)
	}
	clock = clock.Add(time.Second)
	if p, err := a.Acquire(); err != nil || p != 40000 {
		t.Fatalf("want 40000 after its delay, got %d, %v", p, err)
	}
	if a.UsagePct() != 1.0 {
		t.Fatalf("UsagePct = %v", a.UsagePct())
	}
}

// DELETE rồi ANSWER cuộc gọi mới: cuộc gọi mới không nhận lại cổng vừa trả.
func TestDeleteThenAnswer_DoesNotReuseReleasedPort(t *testing.T) {
	start, end := availableUDPRange(t, 4)
	s := newTestServerWithVideo(t, &fakeStarter{}, start, end)
	s.portAlloc.SetReuseDelay(300 * time.Millisecond)

	answer := func(callID string) int {
		t.Helper()
		w := postJSON(t, s, notifyPath(callID), SessionEvent{Event: "ANSWER", SelectedService: "change_background"})
		assertStatus(t, w, http.StatusCreated)
		var resp SessionResponse
		decodeJSON(t, w, &resp)
		return resp.TCoreVideoRTPPort
	}
	first := answer("call-a")
	assertStatus(t, deletePath(t, s, sessionPath("call-a")), http.StatusNoContent)
	deadline := time.Now().Add(2 * time.Second)
	for s.portAlloc.Cooling() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("released port did not start cooling")
		}
		time.Sleep(time.Millisecond)
	}
	if second := answer(testCallID); second == first {
		t.Fatalf("new call got the just-released port %d", first)
	}
	var conns ConnectionsResponse
	decodeJSON(t, getPath(t, s, "/v1/connections"), &conns)
	if conns.RTP.PerSessionOpen != 1 || conns.RTP.PerSessionCooling != 1 {
		t.Fatalf("connections rtp: %+v", conns.RTP)
	}
	assertStatus(t, deletePath(t, s, sessionPath(testCallID)), http.StatusNoContent)
	time.Sleep(400 * time.Millisecond) // cleanup kiểm tra mọi cổng đã trống
}
