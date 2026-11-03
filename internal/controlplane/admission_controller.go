package controlplane

import (
	"runtime"

	"media-ai-gateway/internal/ai"
	"media-ai-gateway/internal/session"
)

// poolQueue là interface tối thiểu mà AdmissionController cần từ WorkerPool.
// *pipeline.WorkerPool implement interface này tự động.
type poolQueue interface {
	QueueLen() int
	QueueCap() int
}

// AdmissionController kiểm tra gateway còn đủ tài nguyên để nhận session mới.
//
// Ngưỡng từ chối (§5.3):
//   - Sessions  ≥ 90% max       → retry sau 5s   (reason: "session_capacity")
//   - Audio job queue ≥ 80%     → retry sau 1s   (reason: "queue_pressure")
//   - AI streams ≥ 95% max      → retry sau 2s   (reason: "ai_stream_capacity")
//   - Không có AI worker alive  → retry sau 3s   (reason: "no_ai_worker")
//   - Heap alloc ≥ memThreshold → retry sau 5s   (reason: "memory_pressure")
//   - RTP port pool cạn         → retry sau 10s  (reason: "port_exhausted")
type AdmissionController struct {
	sessionMgr *session.Manager
	pool       poolQueue
	aiMgr      *ai.Manager
	portAlloc  *PortAllocator

	// Optional; nil/0 → bỏ qua check tương ứng.
	workerRegistry *ai.WorkerRegistry
	memThreshold   uint64 // heap Alloc ngưỡng (bytes); 0 = disabled
}

// NewAdmissionController tạo AdmissionController với các dependency cần kiểm tra.
// portAlloc có thể nil nếu không dùng per-session port allocation.
// pool chấp nhận *pipeline.WorkerPool hoặc bất kỳ type nào implement poolQueue.
func NewAdmissionController(
	sessionMgr *session.Manager,
	pool poolQueue,
	aiMgr *ai.Manager,
	portAlloc *PortAllocator,
) *AdmissionController {
	return &AdmissionController{
		sessionMgr: sessionMgr,
		pool:       pool,
		aiMgr:      aiMgr,
		portAlloc:  portAlloc,
	}
}

// SetWorkerRegistry wires WorkerRegistry để kiểm tra AI reachability.
// Khi được set, readiness sẽ fail nếu không có fresh worker nào.
func (ac *AdmissionController) SetWorkerRegistry(r *ai.WorkerRegistry) { ac.workerRegistry = r }

// SetMemThreshold đặt ngưỡng heap allocation (bytes) cho readiness check.
// 0 vô hiệu hoá kiểm tra memory.
func (ac *AdmissionController) SetMemThreshold(bytes uint64) { ac.memThreshold = bytes }

// CheckWithReason trả về (ok, retryAfterMs, reason).
// reason là empty khi ok=true; ngược lại là tên ngưỡng bị vi phạm.
func (ac *AdmissionController) CheckWithReason() (ok bool, retryAfterMs int64, reason string) {
	if float64(ac.sessionMgr.Count())/float64(ac.sessionMgr.MaxSessions()) >= 0.9 {
		return false, 5000, "session_capacity"
	}
	if cap := ac.pool.QueueCap(); cap > 0 {
		if float64(ac.pool.QueueLen())/float64(cap) >= 0.8 {
			return false, 1000, "queue_pressure"
		}
	}
	if float64(ac.aiMgr.Count())/float64(ac.aiMgr.MaxStreams()) >= 0.95 {
		return false, 2000, "ai_stream_capacity"
	}
	if ac.workerRegistry != nil && len(ac.workerRegistry.List()) == 0 {
		return false, 3000, "no_ai_worker"
	}
	if ac.memThreshold > 0 {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.Alloc >= ac.memThreshold {
			return false, 5000, "memory_pressure"
		}
	}
	if ac.portAlloc != nil && ac.portAlloc.Available() == 0 {
		return false, 10000, "port_exhausted"
	}
	return true, 0, ""
}

// Check trả về (ok, retryAfterMs); backward-compatible với các caller hiện tại.
func (ac *AdmissionController) Check() (ok bool, retryAfterMs int64) {
	ok, retryAfterMs, _ = ac.CheckWithReason()
	return
}
