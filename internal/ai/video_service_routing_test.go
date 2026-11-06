package ai

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"media-ai-gateway/internal/mockai"
)

// 1 dịch vụ ↔ 1 AI: worker gắn Services chỉ phục vụ đúng dịch vụ đó, kể cả khi
// dịch vụ khác dùng cùng AI task. Worker không khai báo Services (config cũ) phục vụ mọi dịch vụ.
func TestSelectVideoTaskServiceIsolation(t *testing.T) {
	r := NewWorkerRegistry(0)
	bg := videoWorker("bg", "video_segment")
	bg.Services = []string{"change_background"}
	blur := videoWorker("blur", "video_segment")
	blur.Services = []string{"blur_background"}
	r.Register(bg)
	r.Register(blur)

	for service, want := range map[string]string{"change_background": "bg", "Blur_Background": "blur"} {
		w, err := r.SelectVideoTask(service, "video_segment", "jpeg", "video_segment")
		if err != nil || w.ID != want {
			t.Fatalf("%s: got %q, %v; want %q", service, w.ID, err, want)
		}
		r.ReleaseVideoTask(w.ID)
	}
	if _, err := r.SelectVideoTask("gesture_recognition", "video_segment", "jpeg", "video_segment"); !errors.Is(err, ErrNoWorkerAvailable) {
		t.Fatalf("service without its own worker must not borrow another service's worker: %v", err)
	}

	legacy := videoWorker("legacy", "video_segment")
	r.Register(legacy)
	if w, err := r.SelectVideoTask("gesture_recognition", "video_segment", "jpeg", "video_segment"); err != nil || w.ID != "legacy" {
		t.Fatalf("legacy worker (no Services) must serve any service: %q, %v", w.ID, err)
	}
}

func TestHasVideoCapacityPerService(t *testing.T) {
	r := NewWorkerRegistry(0)
	gesture := videoWorker("gesture", "gesture_recognition") // MaxStreams 1
	gesture.Services = []string{"gesture_recognition"}
	r.Register(gesture)

	if !r.HasVideoCapacity("gesture_recognition", "gesture_recognition") {
		t.Fatal("free slot not reported")
	}
	if r.HasVideoCapacity("change_background", "video_segment") || r.HasVideoCapacity("other", "gesture_recognition") {
		t.Fatal("capacity reported for a service without its own worker")
	}
	if _, err := r.SelectVideoTask("gesture_recognition", "gesture_recognition", "jpeg", "video_segment"); err != nil {
		t.Fatal(err)
	}
	if r.HasVideoCapacity("gesture_recognition", "gesture_recognition") {
		t.Fatal("full worker reported as available")
	}
	r.ReleaseVideoTask("gesture")
	if !r.HasVideoCapacity("gesture_recognition", "gesture_recognition") {
		t.Fatal("released slot not reported")
	}
}

// Hai gRPC server thật trên loopback, mỗi server là AI của một dịch vụ (cùng task
// video_segment). Dial theo SelectedService phải tới đúng server của dịch vụ đó.
func TestVideoRoutingByServiceRealServers(t *testing.T) {
	opened := make(chan string, 4)
	addrs := map[string]string{}
	for _, label := range []string{"bg-ai", "blur-ai"} {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		label := label
		srv := grpc.NewServer(grpc.StreamInterceptor(func(s any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			opened <- label
			return h(s, ss)
		}))
		mockai.RegisterVideo(srv)
		go srv.Serve(lis)
		t.Cleanup(srv.Stop)
		addrs[label] = lis.Addr().String()
	}
	routes := map[string]string{"change_background": "bg-ai", "blur_background": "blur-ai"}
	registry := NewWorkerRegistry(0)
	for service, label := range routes {
		w := videoWorker("video-service:"+service, "video_segment")
		w.Addr, w.Services = addrs[label], []string{service}
		registry.Register(w)
	}
	pool := NewSharedConnPool(0, 0)
	closePoolConns(t, pool)
	router := NewVideoRoutingDialer(registry, pool.VideoDialFunc())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, service := range []string{"change_background", "blur_background", "change_background"} {
		client, err := router.Dial(ctx, "call-tcore", "call-tcore", VideoStreamConfig{
			SelectedService: service, EffectType: "bg_blur", InferWidth: 8, InferHeight: 4, InferFPS: 5,
		})
		if err != nil {
			t.Fatalf("%s: %v", service, err)
		}
		select {
		case got := <-opened:
			if got != routes[service] {
				t.Fatalf("%s reached %s, want %s", service, got, routes[service])
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no stream reached any server", service)
		}
		client.(*reservedVideoClient).Close()
	}
	if _, err := router.Dial(ctx, "call-tcore", "call-tcore", VideoStreamConfig{SelectedService: "gesture_recognition"}); !errors.Is(err, ErrNoWorkerAvailable) {
		t.Fatalf("service without worker: got %v, want ErrNoWorkerAvailable", err)
	}
	select {
	case got := <-opened:
		t.Fatalf("unexpected stream on %s", got)
	default:
	}
}
