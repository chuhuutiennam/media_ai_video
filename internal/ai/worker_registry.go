package ai

import (
	"errors"
	"slices"
	"strings"
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

// servesVideo: worker phục vụ được dịch vụ service với AI task qua giao thức video cho trước.
// Worker không khai báo Services (kiểu cũ) phục vụ mọi dịch vụ.
func (w WorkerInfo) servesVideo(service, task, format, protocol string) bool {
	return slices.Contains(w.Tasks, task) && slices.Contains(w.MediaTypes, "video") &&
		slices.Contains(w.InputFormats, format) && slices.Contains(w.Protocols, protocol) &&
		(len(w.Services) == 0 || slices.ContainsFunc(w.Services, func(s string) bool { return strings.EqualFold(s, service) }))
}

// SelectVideoTask requires explicit capabilities and atomically reserves a slot.
// Empty/wildcard audio registrations must never be selected for a video RPC.
// service là selectedService của cuộc gọi; worker gắn dịch vụ khác bị loại.
func (r *WorkerRegistry) SelectVideoTask(service, task, format, protocol string) (WorkerInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *WorkerInfo
	for _, w := range r.listFresh(time.Now()) {
		if !w.servesVideo(service, task, format, protocol) || w.atCapacity() {
			continue
		}
		if best == nil || w.loadScore() < best.loadScore() {
			copy := w
			best = &copy
		}
	}
	if best == nil {
		return WorkerInfo{}, ErrNoWorkerAvailable
	}
	w := r.workers[best.ID]
	w.ActiveStreams++
	r.workers[w.ID] = w
	return w, nil
}

// HasVideoCapacity: còn worker fresh phục vụ (service, task) qua VideoStream/Segment (JPEG) với
// slot trống. Chỉ đọc, không giữ slot — slot vẫn được giữ lúc dial (SelectVideoTask).
func (r *WorkerRegistry) HasVideoCapacity(service, task string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, w := range r.listFresh(time.Now()) {
		if w.servesVideo(service, task, "jpeg", "video_segment") && !w.atCapacity() {
			return true
		}
	}
	return false
}

func (r *WorkerRegistry) ReleaseVideoTask(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workers[id]; ok && w.ActiveStreams > 0 {
		w.ActiveStreams--
		r.workers[id] = w
	}
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
