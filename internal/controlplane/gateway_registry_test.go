package controlplane

import (
	"sync"
	"testing"
	"time"
)

func makeInfo(id string, sessions, maxSessions int) GatewayInfo {
	return GatewayInfo{
		ID:          id,
		Addr:        id + ":8080",
		RTPPublicIP: "10.0.0.1",
		Sessions:    sessions,
		MaxSessions: maxSessions,
		AIStreams:    0,
		MaxAIStreams: 100,
	}
}

// TestRegistry_RegisterAndGet kiểm tra register + get cơ bản.
func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewGatewayRegistry(0)
	r.Register(makeInfo("gw-1", 10, 100))

	info, ok := r.Get("gw-1")
	if !ok {
		t.Fatal("Get: expected to find gw-1")
	}
	if info.ID != "gw-1" || info.Sessions != 10 || info.MaxSessions != 100 {
		t.Errorf("unexpected info: %+v", info)
	}
}

// TestRegistry_Register_SetsUpdatedAt kiểm tra UpdatedAt được gán khi Register.
func TestRegistry_Register_SetsUpdatedAt(t *testing.T) {
	r := NewGatewayRegistry(0)
	before := time.Now()
	r.Register(makeInfo("gw-1", 0, 100))
	after := time.Now()

	info, _ := r.Get("gw-1")
	if info.UpdatedAt.Before(before) || info.UpdatedAt.After(after) {
		t.Errorf("UpdatedAt %v out of range [%v, %v]", info.UpdatedAt, before, after)
	}
}

// TestRegistry_Register_UpdatesExisting kiểm tra register lại cùng ID cập nhật entry.
func TestRegistry_Register_UpdatesExisting(t *testing.T) {
	r := NewGatewayRegistry(0)
	r.Register(makeInfo("gw-1", 10, 100))
	r.Register(makeInfo("gw-1", 50, 100))

	info, _ := r.Get("gw-1")
	if info.Sessions != 50 {
		t.Errorf("Sessions: want 50, got %d", info.Sessions)
	}
	if r.Len() != 1 {
		t.Errorf("Len: want 1, got %d", r.Len())
	}
}

// TestRegistry_Deregister xóa node khỏi registry.
func TestRegistry_Deregister(t *testing.T) {
	r := NewGatewayRegistry(0)
	r.Register(makeInfo("gw-1", 0, 100))
	r.Deregister("gw-1")

	if _, ok := r.Get("gw-1"); ok {
		t.Error("expected gw-1 to be removed")
	}
	if r.Len() != 0 {
		t.Errorf("Len: want 0, got %d", r.Len())
	}
}

// TestRegistry_Deregister_NotFound không panic khi xóa ID không tồn tại.
func TestRegistry_Deregister_NotFound(t *testing.T) {
	r := NewGatewayRegistry(0)
	r.Deregister("nonexistent") // must not panic
}

// TestRegistry_Get_NotFound trả về false khi ID không tồn tại.
func TestRegistry_Get_NotFound(t *testing.T) {
	r := NewGatewayRegistry(0)
	_, ok := r.Get("missing")
	if ok {
		t.Error("expected false for missing ID")
	}
}

// TestRegistry_List_Empty trả về slice rỗng khi không có node.
func TestRegistry_List_Empty(t *testing.T) {
	r := NewGatewayRegistry(0)
	if nodes := r.List(); len(nodes) != 0 {
		t.Errorf("List: want 0, got %d", len(nodes))
	}
}

// TestRegistry_List_AllFresh trả về tất cả node còn trong TTL.
func TestRegistry_List_AllFresh(t *testing.T) {
	r := NewGatewayRegistry(time.Minute)
	r.Register(makeInfo("gw-1", 0, 100))
	r.Register(makeInfo("gw-2", 0, 100))
	r.Register(makeInfo("gw-3", 0, 100))

	if got := len(r.List()); got != 3 {
		t.Errorf("List: want 3, got %d", got)
	}
}

// TestRegistry_List_FiltersStale loại bỏ node đã vượt TTL.
func TestRegistry_List_FiltersStale(t *testing.T) {
	r := NewGatewayRegistry(50 * time.Millisecond)

	r.Register(makeInfo("gw-stale", 0, 100))
	time.Sleep(60 * time.Millisecond)
	r.Register(makeInfo("gw-fresh", 0, 100))

	nodes := r.List()
	if len(nodes) != 1 {
		t.Fatalf("List: want 1 fresh node, got %d", len(nodes))
	}
	if nodes[0].ID != "gw-fresh" {
		t.Errorf("expected gw-fresh, got %s", nodes[0].ID)
	}
}

// TestRegistry_List_ZeroTTL_NoExpiry với TTL=0 thì không bao giờ expire.
func TestRegistry_List_ZeroTTL_NoExpiry(t *testing.T) {
	r := NewGatewayRegistry(0)
	r.Register(makeInfo("gw-1", 0, 100))

	// Manually backdate UpdatedAt to simulate old entry.
	r.mu.Lock()
	info := r.nodes["gw-1"]
	info.UpdatedAt = time.Now().Add(-24 * time.Hour)
	r.nodes["gw-1"] = info
	r.mu.Unlock()

	if got := len(r.List()); got != 1 {
		t.Errorf("List: want 1 (no TTL), got %d", got)
	}
}

// TestRegistry_Len_IncludesStale Len() tính cả node stale.
func TestRegistry_Len_IncludesStale(t *testing.T) {
	r := NewGatewayRegistry(1 * time.Millisecond)
	r.Register(makeInfo("gw-1", 0, 100))
	time.Sleep(5 * time.Millisecond)

	// List filters stale, but Len counts all.
	if r.Len() != 1 {
		t.Errorf("Len: want 1, got %d", r.Len())
	}
	if len(r.List()) != 0 {
		t.Error("List: expected 0 after TTL expiry")
	}
}

// TestRegistry_Concurrent_NoRace chạy Register/Deregister/List đồng thời để phát hiện data race.
func TestRegistry_Concurrent_NoRace(t *testing.T) {
	r := NewGatewayRegistry(time.Minute)
	var wg sync.WaitGroup
	const goroutines = 20

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := string(rune('A' + n%26))
			r.Register(GatewayInfo{ID: id, MaxSessions: 100})
			_ = r.List()
			r.Deregister(id)
		}(i)
	}
	wg.Wait()
}
