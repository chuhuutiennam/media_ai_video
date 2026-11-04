package ai

import "media-ai-gateway/internal/ai/proto/legacytest"

// The original server codec is a test fixture, not a second runtime contract.
type videoRequestWire = legacytest.Request
type videoResponseWire = legacytest.Response
type videoProtoCodec = legacytest.Codec
