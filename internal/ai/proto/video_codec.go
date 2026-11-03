// Package proto implements the protobuf transport contract in video.proto.
// It has no media/CGO dependencies, so lab workers remain pure Go.
package proto

import (
	"fmt"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// Method là full gRPC method path phía client gọi cho video segmentation.
const Method = "/video.VideoStream/Segment"

// Request là wire type gửi lên AI worker — tương ứng VideoRequest
// (oneof config/frame) trong api/proto (xem docs, field numbers dưới đây).
type Request struct {
	SessionID string
	StreamID  string
	IsEOS     bool

	// oneof payload — chỉ một trong hai được set.
	HasConfig                         bool
	EffectType                        string
	InferWidth, InferHeight, InferFPS uint32
	Threshold                         float32

	HasFrame       bool
	FrameID        int64
	RTPTimestamp   int64
	TimestampMs    int64
	OriginalWidth  int32
	OriginalHeight int32
	Encoding       string
	ImageData      []byte
}

// Response là wire type nhận từ AI worker — tương ứng VideoResponse{mask}.
type Response struct {
	SessionID string
	StreamID  string
	IsFinal   bool

	HasMask      bool
	FrameID      int64
	RTPTimestamp int64
	MaskWidth    int32
	MaskHeight   int32
	RLECounts    []uint32
	LatencyMs    float32
	Status       string
	ErrorMessage string
}

// Codec encode/decode VideoRequest và VideoResponse theo protobuf
// binary để dùng được cho cả client (Gateway gọi AI worker) lẫn server
// (AI worker, hoặc 1 Go test-server/mock-ai-worker giả lập AI worker).
type Codec struct{}

func (Codec) Name() string { return "proto" }

func (Codec) Marshal(v any) ([]byte, error) {
	switch m := v.(type) {
	case *Request:
		return MarshalRequest(m), nil
	case *Response:
		return MarshalResponse(m), nil
	default:
		return nil, fmt.Errorf("Codec: unsupported marshal type %T", v)
	}
}

func (Codec) Unmarshal(data []byte, v any) error {
	switch m := v.(type) {
	case *Response:
		return UnmarshalResponse(data, m)
	case *Request:
		return UnmarshalRequest(data, m)
	default:
		return fmt.Errorf("Codec: unsupported unmarshal type %T", v)
	}
}

// MarshalRequest encode VideoRequest theo field numbers:
//
//	session_id=1(bytes)  is_eos=2(varint)
//	config=3(embedded VideoConfig: effect_type=1, inference_width=2, inference_height=3, inference_fps=4, threshold=5)
//	frame=4(embedded VideoFrame: frame_id=1, rtp_timestamp=2, original_width=3, original_height=4, encoding=5, image_data=6)
//	stream_id=5(bytes)
func MarshalRequest(m *Request) []byte {
	var b []byte
	if m.SessionID != "" {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendString(b, m.SessionID)
	}
	if m.IsEOS {
		b = protowire.AppendTag(b, 2, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	if m.HasConfig {
		var cfg []byte
		if m.EffectType != "" {
			cfg = protowire.AppendTag(cfg, 1, protowire.BytesType)
			cfg = protowire.AppendString(cfg, m.EffectType)
		}
		if m.InferWidth != 0 {
			cfg = protowire.AppendTag(cfg, 2, protowire.VarintType)
			cfg = protowire.AppendVarint(cfg, uint64(m.InferWidth))
		}
		if m.InferHeight != 0 {
			cfg = protowire.AppendTag(cfg, 3, protowire.VarintType)
			cfg = protowire.AppendVarint(cfg, uint64(m.InferHeight))
		}
		if m.InferFPS != 0 {
			cfg = protowire.AppendTag(cfg, 4, protowire.VarintType)
			cfg = protowire.AppendVarint(cfg, uint64(m.InferFPS))
		}
		if m.Threshold != 0 {
			cfg = protowire.AppendTag(cfg, 5, protowire.Fixed32Type)
			cfg = protowire.AppendFixed32(cfg, math.Float32bits(m.Threshold))
		}
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, cfg)
	}
	if m.HasFrame {
		var fr []byte
		if m.FrameID != 0 {
			fr = protowire.AppendTag(fr, 1, protowire.VarintType)
			fr = protowire.AppendVarint(fr, uint64(m.FrameID))
		}
		if m.RTPTimestamp != 0 {
			fr = protowire.AppendTag(fr, 2, protowire.VarintType)
			fr = protowire.AppendVarint(fr, uint64(m.RTPTimestamp))
		}
		if m.OriginalWidth != 0 {
			fr = protowire.AppendTag(fr, 3, protowire.VarintType)
			fr = protowire.AppendVarint(fr, uint64(int32(m.OriginalWidth)))
		}
		if m.OriginalHeight != 0 {
			fr = protowire.AppendTag(fr, 4, protowire.VarintType)
			fr = protowire.AppendVarint(fr, uint64(int32(m.OriginalHeight)))
		}
		if m.Encoding != "" {
			fr = protowire.AppendTag(fr, 5, protowire.BytesType)
			fr = protowire.AppendString(fr, m.Encoding)
		}
		if m.TimestampMs != 0 {
			fr = protowire.AppendTag(fr, 7, protowire.VarintType)
			fr = protowire.AppendVarint(fr, uint64(m.TimestampMs))
		}

		if len(m.ImageData) > 0 {
			fr = protowire.AppendTag(fr, 6, protowire.BytesType)
			fr = protowire.AppendBytes(fr, m.ImageData)
		}
		b = protowire.AppendTag(b, 4, protowire.BytesType)
		b = protowire.AppendBytes(b, fr)
	}
	if m.StreamID != "" {
		b = protowire.AppendTag(b, 5, protowire.BytesType)
		b = protowire.AppendString(b, m.StreamID)
	}
	return b
}

// UnmarshalResponse decode VideoResponse theo field numbers:
//
//	session_id=1(bytes)  mask=2(embedded VideoMask)  is_final=3(varint)  stream_id=4(bytes)
//
// VideoMask: frame_id=1 rtp_timestamp=2 mask_width=3 mask_height=4
//
//	rle_counts=5(repeated varint, packed hoặc unpacked) latency_ms=6(fixed32)
//	status=7(bytes) error_message=8(bytes)
func UnmarshalResponse(b []byte, m *Response) error {
	*m = Response{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.SessionID = v
			b = b[n:]
		case num == 2 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.HasMask = true
			if err := unmarshalVideoMask(v, m); err != nil {
				return err
			}
			b = b[n:]
		case num == 3 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.IsFinal = v != 0
			b = b[n:]
		case num == 4 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.StreamID = v
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return nil
}

func unmarshalVideoMask(b []byte, m *Response) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.FrameID = int64(v)
			b = b[n:]
		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.RTPTimestamp = int64(v)
			b = b[n:]
		case num == 3 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.MaskWidth = int32(v)
			b = b[n:]
		case num == 4 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.MaskHeight = int32(v)
			b = b[n:]
		case num == 5 && typ == protowire.VarintType: // unpacked repeated varint
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.RLECounts = append(m.RLECounts, uint32(v))
			b = b[n:]
		case num == 5 && typ == protowire.BytesType: // packed repeated varint
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			for len(v) > 0 {
				x, xn := protowire.ConsumeVarint(v)
				if xn < 0 {
					return protowire.ParseError(xn)
				}
				m.RLECounts = append(m.RLECounts, uint32(x))
				v = v[xn:]
			}
			b = b[n:]
		case num == 6 && typ == protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.LatencyMs = math.Float32frombits(v)
			b = b[n:]
		case num == 7 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.Status = v
			b = b[n:]
		case num == 8 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.ErrorMessage = v
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return nil
}

// MarshalResponse encode VideoResponse — chiều NGƯỢC với
// UnmarshalResponse, dùng khi CHÍNH TA đóng vai server (test server giả
// lập AI worker, hoặc 1 mock-ai-worker).
func MarshalResponse(m *Response) []byte {
	var b []byte
	if m.SessionID != "" {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendString(b, m.SessionID)
	}
	if m.HasMask {
		var mask []byte
		if m.FrameID != 0 {
			mask = protowire.AppendTag(mask, 1, protowire.VarintType)
			mask = protowire.AppendVarint(mask, uint64(m.FrameID))
		}
		if m.RTPTimestamp != 0 {
			mask = protowire.AppendTag(mask, 2, protowire.VarintType)
			mask = protowire.AppendVarint(mask, uint64(m.RTPTimestamp))
		}
		if m.MaskWidth != 0 {
			mask = protowire.AppendTag(mask, 3, protowire.VarintType)
			mask = protowire.AppendVarint(mask, uint64(m.MaskWidth))
		}
		if m.MaskHeight != 0 {
			mask = protowire.AppendTag(mask, 4, protowire.VarintType)
			mask = protowire.AppendVarint(mask, uint64(m.MaskHeight))
		}
		for _, rle := range m.RLECounts { // unpacked, đối xứng với nhánh unpacked ở unmarshalVideoMask
			mask = protowire.AppendTag(mask, 5, protowire.VarintType)
			mask = protowire.AppendVarint(mask, uint64(rle))
		}
		if m.LatencyMs != 0 {
			mask = protowire.AppendTag(mask, 6, protowire.Fixed32Type)
			mask = protowire.AppendFixed32(mask, math.Float32bits(m.LatencyMs))
		}
		if m.Status != "" {
			mask = protowire.AppendTag(mask, 7, protowire.BytesType)
			mask = protowire.AppendString(mask, m.Status)
		}
		if m.ErrorMessage != "" {
			mask = protowire.AppendTag(mask, 8, protowire.BytesType)
			mask = protowire.AppendString(mask, m.ErrorMessage)
		}
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, mask)
	}
	if m.IsFinal {
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	if m.StreamID != "" {
		b = protowire.AppendTag(b, 4, protowire.BytesType)
		b = protowire.AppendString(b, m.StreamID)
	}
	return b
}

// UnmarshalRequest decode VideoRequest — chiều NGƯỢC với
// MarshalRequest, dùng khi CHÍNH TA đóng vai server.
func UnmarshalRequest(b []byte, m *Request) error {
	*m = Request{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.SessionID = v
			b = b[n:]
		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.IsEOS = v != 0
			b = b[n:]
		case num == 3 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.HasConfig = true
			m.HasFrame = false
			if err := unmarshalVideoConfig(v, m); err != nil {
				return err
			}
			b = b[n:]
		case num == 4 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.HasFrame = true
			m.HasConfig = false
			if err := unmarshalVideoFrame(v, m); err != nil {
				return err
			}
			b = b[n:]
		case num == 5 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.StreamID = v
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return nil
}

func unmarshalVideoConfig(b []byte, m *Request) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.EffectType = v
			b = b[n:]
		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.InferWidth = uint32(v)
			b = b[n:]
		case num == 3 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.InferHeight = uint32(v)
			b = b[n:]
		case num == 4 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.InferFPS = uint32(v)
			b = b[n:]
		case num == 5 && typ == protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.Threshold = math.Float32frombits(v)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return nil
}

func unmarshalVideoFrame(b []byte, m *Request) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.FrameID = int64(v)
			b = b[n:]
		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.RTPTimestamp = int64(v)
			b = b[n:]
		case num == 3 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.OriginalWidth = int32(v)
			b = b[n:]
		case num == 4 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.OriginalHeight = int32(v)
			b = b[n:]
		case num == 5 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.Encoding = v
			b = b[n:]
		case num == 7 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.TimestampMs = int64(v)
			b = b[n:]
		case num == 6 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			m.ImageData = append([]byte(nil), v...) // copy — v là slice con của buffer decode dùng chung
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return nil
}
