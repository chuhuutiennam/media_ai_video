package ai

import "time"

// WorkerInfo mô tả trạng thái và khả năng của một AI worker node.
// Được cập nhật định kỳ qua Register() (heartbeat từ worker hoặc static config).
type WorkerInfo struct {
	ID           string    // định danh duy nhất
	Addr         string    // gRPC address "host:port"
	Languages    []string  // ngôn ngữ hỗ trợ; ["*"] hoặc [] = không giới hạn
	Tasks        []string  // task hỗ trợ ("speech_to_text","realtime_translation"); ["*"] hoặc [] = không giới hạn
	Models       []string  // model name, e.g. ["faster-whisper-medium","phowhisper-large"]
	MaxStreams    int       // giới hạn stream đồng thời; 0 = không giới hạn
	ActiveStreams int       // số stream đang chạy (từ heartbeat gần nhất)
	GPULoad      float64   // 0..1; 0 = không có GPU hoặc chưa đo
	UpdatedAt    time.Time // thời điểm Register() gần nhất
}

// Supports trả về true nếu worker có thể xử lý yêu cầu với language và task cho trước.
func (w WorkerInfo) Supports(language, task string) bool {
	return matchesCapability(w.Languages, language) && matchesCapability(w.Tasks, task)
}

// loadScore tính điểm tải để so sánh worker (số nhỏ hơn = ít tải hơn).
//
// Công thức: activeRatio*0.7 + GPULoad*0.3 (khi có GPU).
// activeRatio = 0 nếu MaxStreams == 0 (worker không giới hạn).
func (w WorkerInfo) loadScore() float64 {
	var ratio float64
	if w.MaxStreams > 0 {
		ratio = float64(w.ActiveStreams) / float64(w.MaxStreams)
	}
	if w.GPULoad > 0 {
		return ratio*0.7 + w.GPULoad*0.3
	}
	return ratio
}

// atCapacity trả về true nếu worker đã đạt giới hạn stream.
func (w WorkerInfo) atCapacity() bool {
	return w.MaxStreams > 0 && w.ActiveStreams >= w.MaxStreams
}

// matchesCapability kiểm tra value có nằm trong list không.
// List rỗng hoặc chứa "*" nghĩa là hỗ trợ tất cả.
func matchesCapability(list []string, value string) bool {
	if len(list) == 0 {
		return true
	}
	for _, s := range list {
		if s == "*" || s == value {
			return true
		}
	}
	return false
}
