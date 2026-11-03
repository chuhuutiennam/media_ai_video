package controlplane

import "errors"

// ErrNoGatewayAvailable được trả về khi không có gateway nào còn capacity.
var ErrNoGatewayAvailable = errors.New("controlplane: no gateway available")

// GatewaySelector chọn gateway phù hợp nhất để tạo session mới,
// dựa trên trạng thái tài nguyên hiện tại trong GatewayRegistry.
type GatewaySelector struct {
	registry *GatewayRegistry
}

// NewGatewaySelector tạo GatewaySelector dùng registry cho trước.
func NewGatewaySelector(registry *GatewayRegistry) *GatewaySelector {
	return &GatewaySelector{registry: registry}
}

// Select trả về gateway có tải thấp nhất trong số các node còn capacity.
// Gateway bị loại nếu session load ≥ 90% (cùng ngưỡng với AdmissionController).
// Trả về ErrNoGatewayAvailable nếu không có node nào phù hợp.
func (sel *GatewaySelector) Select() (GatewayInfo, error) {
	nodes := sel.registry.List()

	var best GatewayInfo
	bestLoad := 2.0 // sentinel > 1.0
	found := false

	for _, node := range nodes {
		load := node.sessionLoad()
		if load >= 0.9 {
			continue
		}
		if !found || load < bestLoad {
			best = node
			bestLoad = load
			found = true
		}
	}

	if !found {
		return GatewayInfo{}, ErrNoGatewayAvailable
	}
	return best, nil
}

// SelectByID trả về gateway theo ID cụ thể, bất kể tải hiện tại.
// Trả về ErrNoGatewayAvailable nếu ID không tồn tại trong registry.
func (sel *GatewaySelector) SelectByID(id string) (GatewayInfo, error) {
	info, ok := sel.registry.Get(id)
	if !ok {
		return GatewayInfo{}, ErrNoGatewayAvailable
	}
	return info, nil
}
