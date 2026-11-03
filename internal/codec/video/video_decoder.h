#ifndef RTPGW_VIDEO_DECODER_H
#define RTPGW_VIDEO_DECODER_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef void* VideoDecoderHandle;

typedef enum {
    VIDEO_OK              =  0,
    VIDEO_NO_FRAME        =  1,
    VIDEO_PACKET_GAP      =  2,
    VIDEO_DROP_AU         =  3,
    VIDEO_WAIT_IDR        =  4,

    VIDEO_ERR_INVALID_ARGUMENT = -1,
    VIDEO_ERR_INVALID_PAYLOAD  = -2,
    VIDEO_ERR_INVALID_STAPA    = -3,
    VIDEO_ERR_INVALID_FUA      = -4,
    VIDEO_ERR_DECODER_INIT     = -5,
    VIDEO_ERR_DECODE           = -6,
    VIDEO_ERR_CONVERT          = -7,
    VIDEO_ERR_NO_MEMORY        = -8
} VideoStatus;

typedef enum {
    VIDEO_PIX_FMT_UNKNOWN = 0,
    VIDEO_PIX_FMT_YUV420P = 1,
    VIDEO_PIX_FMT_NV12    = 2,
    VIDEO_PIX_FMT_RGB24   = 3
} VideoPixelFormat;

typedef struct {
    uint64_t frame_id;

    uint32_t rtp_timestamp;
    uint64_t extended_rtp_timestamp;
    int64_t  timestamp_ms;

    int width;
    int height;

    VideoPixelFormat pixel_format;

    uint8_t* data;
    int      data_size;

    int stride0;
    int stride1;
    int stride2;
} DecodedVideoFrame;

typedef struct {
    size_t max_nalu_size;
    size_t max_access_unit_size;
    int    max_width;
    int    max_height;
    int    max_ready_frames;
    int    drop_until_idr_on_loss;
} VideoDecoderConfig;

typedef struct {
    uint64_t rtp_packets_input;
    uint64_t single_nalu_packets;
    uint64_t stapa_packets;
    uint64_t fua_packets;
    uint64_t packet_gaps;
    uint64_t corrupted_access_units;
    uint64_t access_units_completed;
    uint64_t idr_frames;
    uint64_t wait_idr_drops;
    uint64_t decode_errors;
    uint64_t decoded_frames;
    uint64_t conversion_errors;
} VideoDecoderStats;

VideoDecoderHandle video_decoder_create(void);
VideoDecoderHandle video_decoder_create_with_config(const VideoDecoderConfig* config);

VideoStatus video_decoder_push_rtp(
    VideoDecoderHandle handle,
    uint16_t           sequence,
    uint32_t           timestamp,
    int                marker,
    const uint8_t*     payload,
    int                payload_size);

VideoStatus video_decoder_receive_frame(
    VideoDecoderHandle handle,
    DecodedVideoFrame* frame);

void video_decoder_release_frame(
    VideoDecoderHandle handle,
    DecodedVideoFrame* frame);

VideoStatus video_decoder_notify_packet_loss(
    VideoDecoderHandle handle,
    uint16_t           expected_seq,
    uint16_t           received_seq);

VideoStatus video_decoder_reset(VideoDecoderHandle handle);

VideoStatus video_decoder_get_stats(
    VideoDecoderHandle  handle,
    VideoDecoderStats*  stats);

void video_decoder_destroy(VideoDecoderHandle handle);

#ifdef __cplusplus
}
#endif

#endif /* RTPGW_VIDEO_DECODER_H */
