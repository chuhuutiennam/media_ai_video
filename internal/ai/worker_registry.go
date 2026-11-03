package ai

import (
	"errors"
	"sync"
	"time"
)

// ErrNoWorkerAvailable được trả về khi không có AI worker nào phù hợp với yêu cầu.
var ErrNoWorkerAvailable = errors.New("ai: no worker available for request")

// WorkerRegistry lưu trữ danh sách AI worker nodes và trạng thái của chúng.
// Thread-safe. Worker không cập nhật trong TTL sẽ bị loại khỏi Select/List.
type WorkerRegistry struct {
	mu      sync.RWMutex
	workers map[string]WorkerInfo
	ttl     time.Duration // 0 = không expire
}

// NewWorkerRegistry tạo registry mới.
// ttl là thời gian tối đa một worker có thể không cập nhật trước khi bị coi là stale.
// ttl = 0 vô hiệu hoá expire.
func NewWorkerRegistry(ttl time.Duration) *WorkerRegistry {
	return &WorkerRegistry{
		workers: make(map[string]WorkerInfo),
		ttl:     ttl,
	}
}

// Register thêm hoặc cập nhật thông tin của một AI worker.
func (r *WorkerRegistry) Register(info WorkerInfo) {
	info.UpdatedAt = time.Now()
	r.mu.Lock()
	r.workers[info.ID] = info
	r.mu.Unlock()
}

// Deregister xóa worker khỏi registry.
func (r *WorkerRegistry) Deregister(id string) {
	r.mu.Lock()
	delete(r.workers, id)
	r.mu.Unlock()
}

// Len trả về tổng số worker đang được theo dõi (kể cả stale).
func (r *WorkerRegistry) Len() int {
	r.mu.RLock()
	n := len(r.workers)
	r.mu.RUnlock()
	return n
}

// List trả về tất cả worker còn fresh (trong TTL).
func (r *WorkerRegistry) List() []WorkerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listFresh(time.Now())
}

// Select chọn worker ít tải nhất có thể xử lý language và task cho trước.
// Loại bỏ worker stale, không hỗ trợ capability, hoặc đã đạt giới hạn stream.
// Trả về ErrNoWorkerAvailable nếu không có worker phù hợp.
func (r *WorkerRegistry) Select(language, task string) (WorkerInfo, error) {
	r.mu.RLock()
	fresh := r.listFresh(time.Now())
	r.mu.RUnlock()

	var best *WorkerInfo
	for i := range fresh {
		w := &fresh[i]
		if !w.Supports(language, task) || w.atCapacity() {
			continue
		}
		if best == nil || w.loadScore() < best.loadScore() {
			best = w
		}
	}
	if best == nil {
		return WorkerInfo{}, ErrNoWorkerAvailable
	}
	return *best, nil
}

// listFresh trả về workers còn fresh tại thời điểm now. Phải gọi dưới r.mu.RLock.
func (r *WorkerRegistry) listFresh(now time.Time) []WorkerInfo {
	out := make([]WorkerInfo, 0, len(r.workers))
	for _, w := range r.workers {
		if r.ttl > 0 && now.Sub(w.UpdatedAt) > r.ttl {
			continue
		}
		out = append(out, w)
	}
	return out
}
