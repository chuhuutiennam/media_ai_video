package controlplane

import (
	"errors"
	"testing"
	"time"
)

// newRegistry tạo registry không expire với các GatewayInfo cho trước.
func newRegistry(infos ...GatewayInfo) *GatewayRegistry {
	r := NewGatewayRegistry(0)
	for _, info := range infos {
		r.Register(info)
	}
	return r
}

func TestSelector_Select_EmptyRegistry(t *testing.T) {
	sel := NewGatewaySelector(newRegistry())
	_, err := sel.Select()
	if !errors.Is(err, ErrNoGatewayAvailable) {
		t.Errorf("want ErrNoGatewayAvailable, got %v", err)
	}
}

func TestSelector_Select_SingleNode_WithCapacity(t *testing.T) {
	info := makeInfo("gw-1", 10, 100) // 10% load
	sel := NewGatewaySelector(newRegistry(info))

	got, err := sel.Select()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "gw-1" {
		t.Errorf("ID: want gw-1, got %s", got.ID)
	}
}

// TestSelector_Select_AllAtCapacity tất cả node ≥ 90% thì từ chối.
func TestSelector_Select_AllAtCapacity(t *testing.T) {
	sel := NewGatewaySelector(newRegistry(
		makeInfo("gw-1", 90, 100),  // 90% — ngưỡng, bị loại
		makeInfo("gw-2", 95, 100),  // 95%
		makeInfo("gw-3", 100, 100), // 100%
	))
	_, err := sel.Select()
	if !errors.Is(err, ErrNoGatewayAvailable) {
		t.Errorf("want ErrNoGatewayAvailable, got %v", err)
	}
}

// TestSelector_Select_LeastLoaded chọn node có load thấp nhất.
func TestSelector_Select_LeastLoaded(t *testing.T) {
	sel := NewGatewaySelector(newRegistry(
		makeInfo("gw-high", 80, 100), // 80%
		makeInfo("gw-low", 20, 100),  // 20% — phải được chọn
		makeInfo("gw-mid", 50, 100),  // 50%
	))

	got, err := sel.Select()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "gw-low" {
		t.Errorf("want gw-low, got %s", got.ID)
	}
}

// TestSelector_Select_SkipsOverloaded loại bỏ node quá ngưỡng, chọn đúng trong số còn lại.
func TestSelector_Select_SkipsOverloaded(t *testing.T) {
	sel := NewGatewaySelector(newRegistry(
		makeInfo("gw-full", 90, 100), // 90% — bị loại
		makeInfo("gw-ok", 40, 100),   // 40% — được chọn
	))

	got, err := sel.Select()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "gw-ok" {
		t.Errorf("want gw-ok, got %s", got.ID)
	}
}

// TestSelector_Select_ExactThreshold kiểm tra biên: 89/100 = 89% được chọn, 90/100 bị loại.
func TestSelector_Select_ExactThreshold(t *testing.T) {
	t.Run("below_threshold_selected", func(t *testing.T) {
		sel := NewGatewaySelector(newRegistry(makeInfo("gw", 89, 100)))
		_, err := sel.Select()
		if err != nil {
			t.Errorf("89%% should be accepted, got: %v", err)
		}
	})

	t.Run("at_threshold_rejected", func(t *testing.T) {
		sel := NewGatewaySelector(newRegistry(makeInfo("gw", 90, 100)))
		_, err := sel.Select()
		if !errors.Is(err, ErrNoGatewayAvailable) {
			t.Errorf("90%% should be rejected, got: %v", err)
		}
	})
}

// TestSelector_Select_ZeroMaxSessions node có MaxSessions=0 bị coi là full.
func TestSelector_Select_ZeroMaxSessions(t *testing.T) {
	sel := NewGatewaySelector(newRegistry(GatewayInfo{ID: "gw", Sessions: 0, MaxSessions: 0}))
	_, err := sel.Select()
	if !errors.Is(err, ErrNoGatewayAvailable) {
		t.Errorf("MaxSessions=0 should be rejected, got: %v", err)
	}
}

// TestSelector_Select_StaleNodesIgnored node stale không được chọn.
func TestSelector_Select_StaleNodesIgnored(t *testing.T) {
	r := NewGatewayRegistry(10 * time.Millisecond)
	r.Register(makeInfo("gw-stale", 0, 100))
	time.Sleep(20 * time.Millisecond)

	sel := NewGatewaySelector(r)
	_, err := sel.Select()
	if !errors.Is(err, ErrNoGatewayAvailable) {
		t.Errorf("stale nodes should not be selected, got: %v", err)
	}
}

func TestSelector_SelectByID_Found(t *testing.T) {
	info := makeInfo("gw-target", 50, 100)
	sel := NewGatewaySelector(newRegistry(info))

	got, err := sel.SelectByID("gw-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "gw-target" {
		t.Errorf("ID: want gw-target, got %s", got.ID)
	}
}

func TestSelector_SelectByID_NotFound(t *testing.T) {
	sel := NewGatewaySelector(newRegistry(makeInfo("gw-1", 0, 100)))
	_, err := sel.SelectByID("gw-missing")
	if !errors.Is(err, ErrNoGatewayAvailable) {
		t.Errorf("want ErrNoGatewayAvailable, got %v", err)
	}
}

// TestSelector_SelectByID_ReturnsEvenIfOverloaded SelectByID không kiểm tra load.
func TestSelector_SelectByID_ReturnsEvenIfOverloaded(t *testing.T) {
	info := makeInfo("gw-full", 100, 100) // 100% — Select() sẽ loại, SelectByID() không
	sel := NewGatewaySelector(newRegistry(info))

	got, err := sel.SelectByID("gw-full")
	if err != nil {
		t.Fatalf("SelectByID should ignore load, got: %v", err)
	}
	if got.ID != "gw-full" {
		t.Errorf("ID: want gw-full, got %s", got.ID)
	}
}
