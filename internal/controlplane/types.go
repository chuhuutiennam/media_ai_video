package controlplane

import (
	"encoding/json"
	"time"
)

// TerminationInfo mô tả một H.248 termination theo 3GPP TS29.176 TerminationInfo schema.
// Medias là mảng MediaInfo (TS29.176 §MediaInfo) — giữ raw để forward-compat, gateway không parse.
type TerminationInfo struct {
	TerminationID string          `json:"terminationId"`
	Medias        json.RawMessage `json:"medias,omitempty"` // []MediaInfo per TS29.176
}

// MediaResourceInfo mô tả một termination resource trong ctrl-result từ DCSF.
type MediaResourceInfo struct {
	ContextID   string          `json:"contextId"`
	Termination TerminationInfo `json:"termination"`
	CallbackURL string          `json:"callbackUrl,omitempty"`

	CallbackURLs map[string]string `json:"callbackUrls,omitempty"` // optional video_segment / gesture_recognition overrides
}

// MediaResources giữ thông tin tCore và tAccess termination từ DCSF ctrl-result.
type MediaResources struct {
	TCore   MediaResourceInfo `json:"tCore"`
	TAccess MediaResourceInfo `json:"tAccess"`
}

// SessionEvent là JSON body cho POST /v1/vonras/call-sessions/{callId}/notify-event.
// Body gửi thẳng các field — không bọc trong wrapper.
// SessionEvent mô tả sự kiện phiên từ DCSF.
type SessionEvent struct {
	CallID           string         `json:"callId"`
	Event            string         `json:"event"` // "BEGIN" | "ANSWER"
	SelectedService  string         `json:"selectedService,omitempty"`
	Direction        string         `json:"direction,omitempty"`
	Role             string         `json:"role,omitempty"`
	BearerCapability string         `json:"bearerCapability,omitempty"` // "AUDIO" | "VIDEO"
	Location         *EventLocation `json:"location,omitempty"`
	Calling          string         `json:"calling,omitempty"`
	Called           string         `json:"called,omitempty"`
	CallbackURL      string         `json:"callbackUrl,omitempty"` // ANSWER only: URL DCAS dùng để POST CALL_CTRL về DCSF
}

// UnmarshalJSON cho phép calling/called là string hoặc array — bỏ qua nếu không phải string.
func (e *SessionEvent) UnmarshalJSON(b []byte) error {
	type raw struct {
		CallID           string          `json:"callId"`
		Event            string          `json:"event"`
		SelectedService  string          `json:"selectedService,omitempty"`
		Direction        string          `json:"direction,omitempty"`
		Role             string          `json:"role,omitempty"`
		BearerCapability string          `json:"bearerCapability,omitempty"`
		Location         *EventLocation  `json:"location,omitempty"`
		Calling          json.RawMessage `json:"calling,omitempty"`
		Called           json.RawMessage `json:"called,omitempty"`
		CallbackURL      string          `json:"callbackUrl,omitempty"`
	}
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	e.CallID = r.CallID
	e.Event = r.Event
	e.SelectedService = r.SelectedService
	e.Direction = r.Direction
	e.Role = r.Role
	e.BearerCapability = r.BearerCapability
	e.Location = r.Location
	e.CallbackURL = r.CallbackURL
	json.Unmarshal(r.Calling, &e.Calling) //nolint:errcheck
	json.Unmarshal(r.Called, &e.Called)   //nolint:errcheck
	return nil
}

// EventLocation mô tả vị trí địa lý của UE trong sự kiện DCSF.
type EventLocation struct {
	AreaNumber string `json:"areaNumber,omitempty"`
	NCGI       string `json:"ncgi,omitempty"`
	TAC        string `json:"tac,omitempty"`
}

// CtrlResultRequest là JSON body cho POST /v1/vonras/call-sessions/{callId}/ctrl-result.
// callbackUrl nằm bên trong mỗi MediaResourceInfo (per-termination), không phải top-level.
type CtrlResultRequest struct {
	CallID         string          `json:"callId"`
	MediaResources *MediaResources `json:"mediaResources,omitempty"`
}

// NonDcMedia mô tả RTP endpoint theo định dạng SDP (dùng cho 3GPP MRM API).
// sdpmLine là nội dung sau "m=" trong SDP m-line.
// sdpaLines là danh sách nội dung sau "a=" của các a-line liên quan.
type NonDcMedia struct {
	SDPMLine  string   `json:"sdpmLine"`
	SDPALines []string `json:"sdpaLines"`
}

// SessionResponse là JSON response cho session endpoints.
// POST .../notify-event (ANSWER) populates GatewayID, RTPIP, RTPPort, LocalNonDcMedia when applicable;
// GET .../call-sessions/{callId} omits those fields.
type SessionResponse struct {
	SessionID  string    `json:"session_id"`
	Status     string    `json:"status"`
	SourceType string    `json:"source_type"`
	Codec      string    `json:"codec,omitempty"`
	SampleRate int       `json:"sample_rate,omitempty"`
	Channels   int       `json:"channels"`
	Language   string    `json:"language,omitempty"`
	Task       string    `json:"task,omitempty"`
	CreatedAt  time.Time `json:"created_at"`

	// Populated on CREATE for raw_rtp sessions with per-session port pool.
	// Each call allocates 2 ports: one for tCore stream, one for tAccess stream.
	GatewayID              string      `json:"gateway_id,omitempty"`
	RTPIP                  string      `json:"rtp_ip,omitempty"`
	TCoreRTPPort           int         `json:"tcore_rtp_port,omitempty"`
	TAccessRTPPort         int         `json:"taccess_rtp_port,omitempty"`
	TCoreLocalNonDcMedia   *NonDcMedia `json:"tcore_local_non_dc_media,omitempty"`
	TAccessLocalNonDcMedia *NonDcMedia `json:"taccess_local_non_dc_media,omitempty"`

	// Video (chỉ populate khi request có bearerCapability: VIDEO).
	TCoreVideoRTPPort           int         `json:"tcore_video_rtp_port,omitempty"`
	TAccessVideoRTPPort         int         `json:"taccess_video_rtp_port,omitempty"`
	TCoreLocalNonDcVideoMedia   *NonDcMedia `json:"tcore_local_non_dc_video_media,omitempty"`
	TAccessLocalNonDcVideoMedia *NonDcMedia `json:"taccess_local_non_dc_video_media,omitempty"`
}

// StatsResponse là JSON response cho GET /v1/stats.
type StatsResponse struct {
	Sessions   int             `json:"sessions"`
	AIStreams  int             `json:"ai_streams"`
	Pool       PoolStats       `json:"pool"`
	Dispatcher DispatcherStats `json:"dispatcher"`
	AI         AIStats         `json:"ai"`
}

// AIStats là thống kê tích lũy của AI Stream Manager.
type AIStats struct {
	TotalSendErrors uint64 `json:"total_send_errors"`
	TotalRecvErrors uint64 `json:"total_recv_errors"`
	TotalRetries    uint64 `json:"total_retries"`
}

// PoolStats là stats của AudioProc Worker Pool.
type PoolStats struct {
	Submitted      uint64 `json:"submitted"`
	Dropped        uint64 `json:"dropped"`
	Processed      uint64 `json:"processed"`
	DecodeErrors   uint64 `json:"decode_errors"`
	ActiveSessions int    `json:"active_sessions"`
	QueueLen       int    `json:"queue_len"`
}

// DispatcherStats là stats của Result Dispatcher.
type DispatcherStats struct {
	Pushed      uint64 `json:"pushed"`
	Dropped     uint64 `json:"dropped"`
	Sent        uint64 `json:"sent"`
	SentPartial uint64 `json:"sent_partial"`
	SentFinal   uint64 `json:"sent_final"`
	SendErrors  uint64 `json:"send_errors"`
	QueueLen    int    `json:"queue_len"`
}

// ErrorResponse là JSON body khi có lỗi.
type ErrorResponse struct {
	Error        string `json:"error"`
	RetryAfterMs int64  `json:"retry_after_ms,omitempty"`
}

// ConnectionsResponse là JSON response cho GET /v1/connections.
type ConnectionsResponse struct {
	AIWorkers []AIWorkerConn `json:"ai_workers"`
	Callback  CallbackConn   `json:"callback"`
	RTP       RTPConnSummary `json:"rtp"`
}

// AIWorkerConn mô tả trạng thái kết nối gRPC tới một AI worker.
type AIWorkerConn struct {
	Addr         string         `json:"addr"`
	State        string         `json:"state"`          // IDLE | CONNECTING | READY | TRANSIENT_FAILURE | SHUTDOWN
	ActiveStream int            `json:"active_streams"` // AI video: slot đang giữ trên địa chỉ này; còn lại: tổng stream audio
	Latency      AILatencyStats `json:"latency"`
	Services     []string       `json:"services,omitempty"` // AI video: dịch vụ (selectedService) dùng địa chỉ này; rỗng = mọi dịch vụ (config cũ)
	Tasks        []string       `json:"tasks,omitempty"`    // AI video: task trên địa chỉ này
}

// AILatencyStats là thống kê latency của AI worker.
type AILatencyStats struct {
	// First-result: thời gian từ khi stream mở đến khi nhận result đầu tiên.
	AvgFirstResultMs int64 `json:"avg_first_result_ms"` // trung bình qua các stream đã hoàn thành
}

// CallbackConn mô tả trạng thái HTTP/2 connection tới callback server.
type CallbackConn struct {
	URL          string `json:"url"`
	Connected    bool   `json:"connected"`
	Error        string `json:"error,omitempty"`
	PreconnectAt string `json:"preconnect_at,omitempty"` // RFC3339
}

// RTPConnSummary tóm tắt số lượng kết nối RTP.
type RTPConnSummary struct {
	PerSessionOpen     int  `json:"per_session_open"`     // số port đang cấp phát (= session đang dùng per-session port)
	PerSessionCapacity int  `json:"per_session_capacity"` // tổng số port trong pool; 0 nếu pool không cấu hình
	SharedIngress      bool `json:"shared_ingress"`       // true nếu shared UDP ingress đang chạy
}
