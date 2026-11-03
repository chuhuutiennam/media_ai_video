package ai

import videopb "media-ai-gateway/internal/ai/proto"

const videoSegmentRPC = videopb.Method

type videoRequestWire = videopb.Request
type videoResponseWire = videopb.Response
type videoProtoCodec = videopb.Codec
