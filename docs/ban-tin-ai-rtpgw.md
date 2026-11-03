# Mô tả bản tin giữa AI engine và RTPGW

## 1. Kênh truyền và trình tự bản tin

- RPC `/video.VideoStream/Segment`, dùng **gRPC bidirectional streaming**, dữ liệu Protobuf.
- Mở một RPC, gửi `config` một lần rồi gửi nhiều `frame` trên cùng RPC. Mỗi frame không tạo một kết nối hoặc HTTP POST mới.
- Request chỉ có một payload `config` hoặc `frame` (`oneof`); ngoại lệ là EOS chỉ có ID và `is_eos=true`.
- Worker xử lý tuần tự từng frame trong một RPC. Mỗi frame xử lý xong trả một response, kể cả lỗi frame được bắt. Lỗi làm dừng RPC có thể không trả response cho frame đang chờ.
- Config hợp lệ không có response xác nhận riêng. Client phải đọc nhánh nhận để phát hiện lỗi RPC; gửi config thành công chưa chứng minh worker đã chấp nhận config.

```text
RTPGW                                      AI worker
  |---- config ------------------------------>| Kiểm tra, tạo segmenter
  |---- frame 1 ----------------------------->|
  |<--- mask frame 1, is_final=true -----------|
  |---- frame 2 ----------------------------->|
  |<--- mask frame 2, is_final=true -----------|
  |---- is_eos=true, không payload ---------->|
  |<--- RPC kết thúc bình thường -------------|
```

Các JSON dưới đây là **biểu diễn minh họa message Protobuf**, giữ tên trường `snake_case` và viết số nguyên trực tiếp để dễ đọc; không phải HTTP JSON API. `<BYTES_JPEG_...>` là chỗ thay bằng bytes JPEG thật. Trên gRPC, không gửi chuỗi placeholder hoặc chuỗi base64 làm bytes ảnh. Nếu dùng công cụ chuyển đổi ProtoJSON, tuân theo bộ chuyển đổi của công cụ: bytes biểu diễn bằng base64, `int64` thường biểu diễn bằng chuỗi.

Proto3 có giá trị mặc định cho scalar bị bỏ qua: số `0`, bool `false`, string `""`; repeated field mặc định rỗng. Log JSON có thể không hiện trường mặc định. Thiếu `error_message` khi thành công tương đương chuỗi rỗng; **thiếu `status` không tương đương `"ok"`**.

## 2. Các trường RTPGW → AI

### 2.1. Envelope `VideoRequest`

| Trường | Kiểu Protobuf | Ý nghĩa và cách dùng | Ví dụ |
| --- | --- | --- | --- |
| `session_id` | `string` | ID phiên, bắt buộc khác rỗng ở message đầu; giữ nguyên trong toàn bộ RPC. AI echo vào response. | `"video-lab-tcore"` |
| `stream_id` | `string` | ID luồng media; giữ nguyên trong toàn bộ RPC, AI echo lại. Có thể trùng `session_id`. Worker hiện tại chấp nhận rỗng, nhưng nên đặt rõ để định tuyến kết quả. | `"video-lab-tcore"` |
| `is_eos` | `bool` | `false`: tiếp tục stream. `true` với frame: trả kết quả frame rồi đóng RPC. `true` không payload: đóng RPC, không sinh mask. Config đầu không được có `is_eos=true`. | `false` |
| `config` | `VideoConfig` | Payload message đầu tiên; không gửi lại giữa RPC. Muốn đổi cấu hình phải mở RPC mới. | Mục 5.1 |
| `frame` | `VideoFrame` | Payload ảnh cần segmentation sau config; không đi cùng `config`. | Mục 5.2 |

### 2.2. Payload `VideoConfig`

| Trường | Kiểu Protobuf | Ý nghĩa, đơn vị và ràng buộc | Ví dụ |
| --- | --- | --- | --- |
| `effect_type` | `string` | Chỉ nhận `"bg_blur"`, `"bg_replace"`, `"bg_remove"`. Worker hiện tại đều trả mask với cùng quy trình segmentation; bên xử lý ảnh đầu ra thực hiện làm mờ/thay/xóa nền. | `"bg_replace"` |
| `inference_width` | `uint32` | Chiều rộng JPEG gửi AI, pixel; worker chấp nhận `1..4096`. Kích thước ảnh thực phải khớp cấu hình và phù hợp model triển khai. | `256` |
| `inference_height` | `uint32` | Chiều cao JPEG gửi AI, pixel; worker chấp nhận `1..4096`. | `256` |
| `inference_fps` | `uint32` | FPS mục tiêu sau khi RTPGW lấy mẫu. Worker hiện tại không kiểm tra hoặc tự giới hạn tốc độ theo trường này; RTPGW chịu trách nhiệm lấy mẫu và kiểm soát hàng đợi. | `5` |
| `threshold` | `float` | Foreground khi `confidence > threshold`. Giá trị hữu hạn trong `[0,1]`; `0` hoặc bỏ trường dùng mặc định `0.5`, nên không dùng `0` để yêu cầu ngưỡng thực bằng 0. | `0.5` |

### 2.3. Payload `VideoFrame`

| Trường | Kiểu Protobuf | Ý nghĩa, đơn vị và cách xử lý | Ví dụ |
| --- | --- | --- | --- |
| `frame_id` | `int64` | ID frame do RTPGW cấp tăng dần để khớp với kết quả; có thể có khoảng trống do bỏ frame. Worker chỉ echo, không kiểm tra thứ tự/trùng ID. RTPGW bảo đảm ID không trùng trong phạm vi ghép kết quả. | `1` |
| `rtp_timestamp` | `int64` | Timestamp RTP gốc 32 bit, giá trị `0..4294967295`, có thể wrap về 0. Dù truyền bằng `int64`, đây không phải timestamp đã mở rộng hoặc milliseconds. Worker chỉ echo. | `90000` |
| `original_width` | `int32` | Chiều rộng frame gốc sau decode, trước resize, pixel. RTPGW nên gửi giá trị dương và lưu để scale mask; worker hiện tại không dùng trường này để kiểm tra JPEG. | `1280` |
| `original_height` | `int32` | Chiều cao frame gốc, pixel; cùng quy tắc với `original_width`. | `720` |
| `encoding` | `string` | Phải đúng chuỗi `"jpeg"`, phân biệt hoa/thường. Bytes ảnh thực cũng phải là JPEG. | `"jpeg"` |
| `image_data` | `bytes` | Toàn bộ bytes một JPEG đã resize đúng `inference_width × inference_height`; không phải RTP packet, H.264 hoặc RGB thô. | `<BYTES_JPEG_256x256>` |
| `timestamp_ms` | `int64` | Thời gian media, milliseconds, cho MediaPipe; không phải Unix time. Frame đầu có thể là `0`; các frame sau phải tăng nghiêm ngặt trong RPC. Response không echo trường này; RTPGW cần lưu nếu dùng về sau. | `0`, `200`, `400` |

Với H.264 clock 90 kHz trong gateway hiện tại, thời gian tính từ RTP timestamp **đã mở rộng qua wrap-around**:

```text
timestamp_ms = floor((extended_rtp_timestamp - base_extended_rtp_timestamp) * 1000 / 90000)
```

Ví dụ RTP `90000 → 108000 → 126000` tương ứng `0 → 200 → 400 ms` nếu mốc đầu là `90000`. Frame đầu thực sự gửi AI có thể có `timestamp_ms > 0` nếu RTPGW đã bỏ qua các frame trước. Không tính trực tiếp từ timestamp 32 bit khi nó wrap.

**Mốc kiểm tra timestamp:** `last_timestamp` ban đầu là `-1`, được cập nhật ngay sau khi `segment_async` nhận frame thành công, trước kết quả callback. Lỗi callback/RLE vẫn có thể đã tiêu thụ timestamp; lỗi encoding/JPEG xảy ra trước đó thì chưa cập nhật mốc. Khuyến nghị RTPGW luôn dùng timestamp mới lớn hơn cho frame kế tiếp, không gửi lại cùng timestamp sau lỗi.

## 3. Các trường AI → RTPGW

### 3.1. Envelope `VideoResponse`

| Trường | Kiểu Protobuf | Ý nghĩa và cách RTPGW xử lý | Ví dụ |
| --- | --- | --- | --- |
| `session_id` | `string` | Echo từ request của frame; kiểm tra đúng phiên đang chờ. | `"video-lab-tcore"` |
| `stream_id` | `string` | Echo từ request; kiểm tra đúng luồng đang chờ. | `"video-lab-tcore"` |
| `is_final` | `bool` | Worker hiện tại luôn trả `true`: hoàn tất kết quả **của frame này**, không yêu cầu đóng stream. Tiếp tục đọc đến EOF/lỗi RPC. Worker chưa có kết quả trung gian `false`. | `true` |
| `mask` | `VideoMask` | Kết quả hoặc lỗi của frame; chỉ dùng mask khi `status="ok"` và dữ liệu hợp lệ. | Mục 5 |

Lưu ý: comment `is_final` trong proto hiện còn mô tả “mask cuối của stream”; hành vi worker và client nhận hiện tại là trả/nhận nhiều response `is_final=true` trên cùng RPC. Khi tích hợp worker này, dùng ngữ nghĩa theo frame nêu trên.

### 3.2. Payload `VideoMask`

| Trường | Kiểu Protobuf | Ý nghĩa và cách RTPGW xử lý | Ví dụ |
| --- | --- | --- | --- |
| `frame_id` | `int64` | Echo `frame.frame_id`; khóa ghép kết quả chính trong cùng phiên/luồng. | `1` |
| `rtp_timestamp` | `int64` | Echo `frame.rtp_timestamp`; đối chiếu frame đã lưu, không dùng riêng trường này làm khóa duy nhất. | `90000` |
| `mask_width` | `int32` | Chiều rộng mask, pixel; khi thành công phải dương. Dùng kích thước response để giải mã, không lấy độ rộng ảnh gốc. | `256` |
| `mask_height` | `int32` | Chiều cao mask, pixel; khi thành công phải dương. Worker lấy kích thước từ mask model trả về; cấu hình mẫu cho mask `256×256`. | `256` |
| `rle_counts` | `repeated uint32` | Độ dài các run của mask nhị phân theo row-major. Khi thành công, tổng counts bằng `mask_width × mask_height`. | `[32768, 32768]` |
| `latency_ms` | `float` | Milliseconds xử lý phía worker. Hiện đo từ trước kiểm tra frame, qua decode JPEG/chờ inference/tạo RLE đến trước yield response; không bao gồm truyền mạng và thời gian chờ frame được lấy ra xử lý. Không dùng làm timestamp media hoặc độ trễ end-to-end. | `12.5` |
| `status` | `string` | Worker hiện tại trả `"ok"` hoặc `"error"`. RTPGW chỉ chấp nhận mask có `"ok"`; giá trị khác/rỗng là kết quả không sử dụng được. | `"ok"` |
| `error_message` | `string` | Mô tả lỗi khi `status="error"`; thành công thường rỗng/bị bỏ khỏi log. Dùng chẩn đoán, không phân nhánh nghiệp vụ dựa hoàn toàn vào chuỗi lỗi thư viện. | `"encoding must be jpeg"` |

Response không có kích thước ảnh gốc, `timestamp_ms`, `effect_type` hay ảnh gốc; RTPGW phải giữ dữ liệu cần thiết từ request/config. Khi `status="error"`, kích thước/counts có thể rỗng hoặc mới được gán một phần; kích thước khác 0 không chứng minh mask hợp lệ.

## 4. Giải mã RLE và áp mask

Mask nhị phân: background `0`, foreground `1`; không phải confidence/alpha nhiều mức. Worker đã áp threshold trước khi mã hóa.

- Duyệt từng hàng từ trái sang phải, từ trên xuống dưới (**row-major**).
- Run chỉ số 0 luôn là background, tiếp theo foreground rồi luân phiên. Nếu pixel đầu là foreground, run đầu bằng `0`; vẫn đổi giá trị sau run có độ dài 0.
- Run có thể đi qua ranh giới hàng; không reset giá trị ở đầu mỗi hàng.
- Pixel thứ `i` có tọa độ `y = i // mask_width`, `x = i % mask_width`.

| Trường hợp | Kích thước | `rle_counts` | Mask sau giải mã, từng hàng |
| --- | --- | --- | --- |
| Toàn background | `3×2` | `[6]` | `0 0 0` / `0 0 0` |
| Toàn foreground | `3×2` | `[0, 6]` | `1 1 1` / `1 1 1` |
| Hỗn hợp, pixel đầu foreground | `3×2` | `[0, 1, 3, 1, 1]` | `1 0 0` / `0 1 0` |
| Mẫu response mục 5.2 | `256×256` | `[32768, 32768]` | 128 hàng đầu bằng 0, 128 hàng cuối bằng 1 |
| Sai: thiếu pixel | `3×2` | `[2, 3]` | Tổng 5 khác 6: bỏ kết quả |
| Sai: thừa pixel | `3×2` | `[2, 5]` | Tổng 7 khác 6: bỏ kết quả |

Ví dụ `bg_replace`: mask bằng 1 giữ pixel ảnh gốc, bằng 0 lấy pixel ảnh nền thay thế. `bg_blur` làm mờ vùng 0; `bg_remove` có thể đặt alpha vùng 0 bằng 0. Đây là bước của bên ghép ảnh, không phải nội dung AI gửi về.

## 5. Ví dụ theo từng trường hợp

Dùng chung `session_id=stream_id="video-lab-tcore"`, inference `256×256`, ảnh gốc `1280×720`. Mask và thời gian xử lý chỉ minh họa, không phải kết quả model bảo đảm cho một ảnh cụ thể.

### 5.1. Mở stream, gửi cấu hình

RTPGW → AI, message đầu tiên:

```json
{
  "session_id": "video-lab-tcore",
  "stream_id": "video-lab-tcore",
  "is_eos": false,
  "config": {
    "effect_type": "bg_replace",
    "inference_width": 256,
    "inference_height": 256,
    "inference_fps": 5,
    "threshold": 0.5
  }
}
```

Config hợp lệ: AI chờ frame, không trả mask/ACK. Thay `effect_type` bằng `"bg_blur"` hoặc `"bg_remove"` cũng dùng cùng định dạng response mask. Contract không có trường gửi ảnh nền thay thế sang worker.

### 5.2. Frame thành công

RTPGW → AI:

```json
{
  "session_id": "video-lab-tcore",
  "stream_id": "video-lab-tcore",
  "is_eos": false,
  "frame": {
    "frame_id": 1,
    "rtp_timestamp": 90000,
    "original_width": 1280,
    "original_height": 720,
    "encoding": "jpeg",
    "image_data": "<BYTES_JPEG_256x256>",
    "timestamp_ms": 0
  }
}
```

AI → RTPGW:

```json
{
  "session_id": "video-lab-tcore",
  "stream_id": "video-lab-tcore",
  "is_final": true,
  "mask": {
    "frame_id": 1,
    "rtp_timestamp": 90000,
    "mask_width": 256,
    "mask_height": 256,
    "rle_counts": [32768, 32768],
    "latency_ms": 12.5,
    "status": "ok"
  }
}
```

RTPGW tìm frame 1 đúng phiên/luồng, đối chiếu RTP timestamp, kiểm tra RLE rồi giải mã/scale mask. `is_final=true` hoàn tất frame 1; tiếp tục gửi frame 2, 3…

### 5.3. Thành công nhưng không có foreground hoặc toàn foreground

Giữ envelope và các trường khác như response 5.2, thay `rle_counts`:

| Kết quả | `mask.rle_counts` | RTPGW xử lý |
| --- | --- | --- |
| Không pixel nào là foreground | `[65536]` | `status="ok"`, mask hợp lệ toàn 0; với thay nền, toàn ảnh lấy từ nền thay thế. Không coi là lỗi AI. |
| Mọi pixel đều là foreground | `[0, 65536]` | `status="ok"`, mask hợp lệ toàn 1; giữ toàn bộ ảnh gốc. |

Không nhầm `rle_counts=[]` với “không có người”: mảng rỗng là mask không hợp lệ khi kích thước dương.

### 5.4. Lỗi frame: JPEG sai kích thước

RTPGW → AI, cố tình gửi JPEG `320×240` dù config yêu cầu `256×256`:

```json
{
  "session_id": "video-lab-tcore",
  "stream_id": "video-lab-tcore",
  "is_eos": false,
  "frame": {
    "frame_id": 2,
    "rtp_timestamp": 108000,
    "original_width": 1280,
    "original_height": 720,
    "encoding": "jpeg",
    "image_data": "<BYTES_JPEG_320x240_SAI_KICH_THUOC_CONFIG>",
    "timestamp_ms": 200
  }
}
```

AI → RTPGW:

```json
{
  "session_id": "video-lab-tcore",
  "stream_id": "video-lab-tcore",
  "is_final": true,
  "mask": {
    "frame_id": 2,
    "rtp_timestamp": 108000,
    "mask_width": 0,
    "mask_height": 0,
    "rle_counts": [],
    "latency_ms": 0.4,
    "status": "error",
    "error_message": "JPEG dimensions do not match config"
  }
}
```

RTPGW ghi lỗi frame 2, không giải mã/áp mask này. RPC vẫn mở vì `is_eos=false`; có thể gửi frame 3 với JPEG đúng `256×256`, RTP timestamp `126000`, `timestamp_ms=400`. Response frame 3 dùng envelope thành công như 5.2 và echo ID/timestamp của frame 3.

### 5.5. Các lỗi frame khác

Mỗi dòng là **tình huống độc lập**, dùng request/response mẫu 5.4 rồi thay dữ liệu gây lỗi và `error_message` như bảng. Response echo ID/timestamp request lỗi, `is_final=true`, `status="error"`; `latency_ms` đo thực tế. Các lỗi trước khi gán shape mask trả kích thước `0×0`, counts `[]`.

| Trường hợp và đầu vào ví dụ | `mask.error_message` | Dữ liệu mask và cách RTPGW xử lý |
| --- | --- | --- |
| `encoding="png"` hoặc `"JPEG"` | `encoding must be jpeg` | `0×0`, `[]`; sửa encoding và gửi JPEG thật ở frame sau. |
| Sau frame timestamp `200` đã được submit, gửi frame mới với `timestamp_ms=200` hoặc `199` | `timestamp_ms must increase within a stream` | `0×0`, `[]`; gửi timestamp lớn hơn `200`, ví dụ `400`. |
| `image_data` rỗng hoặc không giải mã được | Chi tiết Pillow, ví dụ thông báo chứa `cannot identify image file` | `0×0`, `[]`; sửa JPEG. Nội dung lỗi phụ thuộc thư viện, không so sánh nguyên chuỗi. |
| `encoding="jpeg"` nhưng bytes là PNG `256×256` hợp lệ | `JPEG dimensions do not match config` | `0×0`, `[]`; chuỗi này cũng dùng cho sai định dạng ảnh thực, không chỉ sai kích thước. |
| Model trả 3 confidence masks, chưa đặt `--foreground-index` | `expected 1 or 2 confidence masks; multiclass model requires --foreground-index` | `0×0`, `[]`; sửa cấu hình worker. Nhánh này cũng xuất hiện khi không có confidence mask. |
| `--foreground-index=2`, model chỉ trả 2 confidence masks | `foreground-index outside confidence masks` | `0×0`, `[]`; chọn index hợp lệ ở worker, gửi lại dữ liệu không tự khắc phục. |
| Confidence plane có shape `(256, 256, 2)` | `unexpected confidence mask shape: (256, 256, 2)` | `0×0`, `[]`; chỉ hỗ trợ plane `H×W` hoặc `H×W×1`; kiểm tra model/đầu ra. |
| Foreground plane shape `(0, 256)`, rỗng ở bước RLE | `empty mask` | Width `256`, height `0`, counts `[]`; shape đã được gán trước lỗi. Bỏ mask và kiểm tra model. |
| `ValueError`, `OSError`, `RuntimeError` khác trong xử lý frame; ví dụ `RuntimeError("inference failed")` | `inference failed` trong ví dụ, tổng quát là `str(exception)` | Kích thước/counts tùy bước lỗi; luôn bỏ mask khi status lỗi. Đây là ví dụ ngoại lệ, không phải chuỗi lỗi cố định của model. |

Worker yield các response lỗi được bắt rồi tiếp tục nếu `is_eos=false`. RPC có thể nhận frame tiếp theo không có nghĩa lỗi model đã được khắc phục. Nếu request lỗi mang `is_eos=true`, worker trả response lỗi rồi kết thúc RPC.

### 5.6. Kết thúc stream

**Cách A — EOS riêng, không frame:**

```json
{
  "session_id": "video-lab-tcore",
  "stream_id": "video-lab-tcore",
  "is_eos": true
}
```

Worker đóng RPC bình thường, không trả thêm mask/EOS ACK. RTPGW tiếp tục đọc các response còn chờ đến EOF, không chờ response riêng cho EOS.

**Cách B — đánh dấu frame cuối:**

```json
{
  "session_id": "video-lab-tcore",
  "stream_id": "video-lab-tcore",
  "is_eos": true,
  "frame": {
    "frame_id": 3,
    "rtp_timestamp": 126000,
    "original_width": 1280,
    "original_height": 720,
    "encoding": "jpeg",
    "image_data": "<BYTES_JPEG_256x256>",
    "timestamp_ms": 400
  }
}
```

Worker trả kết quả frame 3 (`ok` hoặc lỗi frame được bắt) rồi đóng RPC. Nếu gặp lỗi làm dừng RPC trong lúc xử lý thì vẫn có thể không có mask cuối.

**Cách C — half-close phía gửi:** sau frame cuối, gọi `CloseSend()` và tiếp tục nhận đến EOF. Worker xử lý hết request đã nhận rồi kết thúc khi iterator request hết dữ liệu. Không hủy context ngay sau `CloseSend()` nếu còn cần kết quả cuối.
