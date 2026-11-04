package ai

import (
	"context"
	"io"
	"testing"

	"media-ai-gateway/internal/pipeline"
)

type taskResultClient struct{ results []pipeline.RecognitionResult }

func (*taskResultClient) Send(pipeline.VideoFrame) error { return nil }
func (*taskResultClient) CloseSend() error               { return nil }
func (c *taskResultClient) Recv() (pipeline.RecognitionResult, error) {
	if len(c.results) == 0 {
		return pipeline.RecognitionResult{}, io.EOF
	}
	r := c.results[0]
	c.results = c.results[1:]
	return r, nil
}

func TestMixedSourceRoutesGesturePastFullMaskResultQueue(t *testing.T) {
	masks, gestures := make(chan pipeline.RecognitionResult, 1), make(chan pipeline.RecognitionResult, 1)
	masks <- pipeline.RecognitionResult{Kind: "video_mask", Seq: 1}
	totals := &videoCounters{}
	s := &VideoStream{sourceMode: true, gestureResultOut: gestures, totals: totals}
	client := &taskResultClient{results: []pipeline.RecognitionResult{
		{Kind: "video_mask", Seq: 2},
		{Kind: "video_gesture", Seq: 3},
	}}
	s.runRecv(context.Background(), client, masks)
	select {
	case r := <-gestures:
		if r.Seq != 3 {
			t.Fatal(r)
		}
	default:
		t.Fatal("gesture blocked/dropped behind full mask queue")
	}
	if totals.maskDropped.Load() != 1 || totals.gestureDropped.Load() != 0 {
		t.Fatal("wrong task drop accounting")
	}
}
