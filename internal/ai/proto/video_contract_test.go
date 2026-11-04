package proto

import (
	"bytes"
	"testing"

	"media-ai-gateway/internal/ai/proto/legacytest"

	protobuf "google.golang.org/protobuf/proto"
)

// The generated shared schema must still interoperate with the deployed legacy
// segmentation codec. In particular, adding a typed result must preserve mask=2.
func TestSharedContractPreservesLegacySegmentationWire(t *testing.T) {
	request := &VideoRequest{SessionId: "call", StreamId: "video", Payload: &VideoRequest_Frame{Frame: &VideoFrame{
		FrameId: 42, RtpTimestamp: 0xffffffff, OriginalWidth: 1280, OriginalHeight: 720,
		TimestampMs: 123, Encoding: "jpeg", ImageData: []byte{0xff, 0xd8, 0xff, 0xd9},
		ExtendedRtpTimestamp: 0x1ffffffff, Width: 640, Height: 360,
	}}}
	wire, err := protobuf.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var legacy legacytest.Request
	if err := legacytest.UnmarshalRequest(wire, &legacy); err != nil {
		t.Fatal(err)
	}
	if !legacy.HasFrame || legacy.FrameID != 42 || legacy.RTPTimestamp != 0xffffffff || legacy.TimestampMs != 123 ||
		legacy.OriginalWidth != 1280 || legacy.OriginalHeight != 720 || legacy.Encoding != "jpeg" || !bytes.Equal(legacy.ImageData, request.GetFrame().ImageData) {
		t.Fatalf("legacy request changed: %+v", legacy)
	}
	var decoded VideoRequest
	if err := protobuf.Unmarshal(legacytest.MarshalRequest(&legacy), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GetFrame().FrameId != 42 || decoded.GetFrame().TimestampMs != 123 || decoded.StreamId != "video" {
		t.Fatal("legacy request no longer readable")
	}

	response := &VideoResponse{SessionId: "call", StreamId: "video", IsFinal: true, AiService: "segmentation",
		Result: &VideoResponse_Mask{Mask: &VideoMask{FrameId: 42, MaskWidth: 2, MaskHeight: 2, RleCounts: []uint32{0, 4}, Status: "ok"}}}
	wire, err = protobuf.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var old legacytest.Response
	if err := legacytest.UnmarshalResponse(wire, &old); err != nil {
		t.Fatal(err)
	}
	if !old.HasMask || old.FrameID != 42 || old.MaskWidth != 2 || old.Status != "ok" || len(old.RLECounts) != 2 {
		t.Fatalf("legacy mask changed: %+v", old)
	}
	var out VideoResponse
	if err := protobuf.Unmarshal(legacytest.MarshalResponse(&old), &out); err != nil {
		t.Fatal(err)
	}
	if out.GetMask() == nil || out.GetGesture() != nil || out.GetMask().FrameId != 42 {
		t.Fatal("legacy mask no longer readable")
	}
}
