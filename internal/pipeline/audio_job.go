package pipeline

// AudioJob là đơn vị công việc gửi vào Audio Pipeline Worker Pool.
// Đại diện cho một MediaPacket đã qua jitter buffer, sẵn sàng decode/resample/chunk.
type AudioJob struct {
	SessionID string
	Packet    MediaPacket
}
