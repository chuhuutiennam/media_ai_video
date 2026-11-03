## 2. Supported Media Setup Modes

### 2.1 HTTP Proxy Mode

In HTTP Proxy mode, MF acts as an intermediary that:

- Terminates DTLS from UE
- Acts as an HTTP proxy
- Forwards HTTP traffic to DC Application Server
- Maintains media resource allocation for both UE side and application side

#### Protocol Stack

UE → SCTP over DTLS → MF → HTTP/TCP/UDP/SCTP → DC Application Server

#### Key Characteristics

- MF does NOT expose WebRTC semantics to application server
- MF translates Data Channel traffic into HTTP requests
- Supports both Bootstrap DC and Application DC

---

### 2.2 Transparent Forwarding Mode (optional)

In this mode:

- MF forwards SCTP data without interpreting payload
- Minimal transformation of media traffic
- Used when no protocol translation is required