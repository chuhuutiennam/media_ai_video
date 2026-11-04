// video-ai-engine exposes one source RPC and isolates the two legacy model workers.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"media-ai-gateway/internal/videoengine"
)

func main() {
	cfg := videoengine.DefaultConfig()
	addr := flag.String("addr", "127.0.0.1:50054", "source gRPC listen address")
	metrics := flag.String("metrics-addr", "127.0.0.1:9103", "HTTP metrics address")
	flag.StringVar(&cfg.SegmentationTarget, "segmentation-target", "", "legacy segmentation endpoint")
	flag.StringVar(&cfg.GestureTarget, "gesture-target", "", "legacy gesture endpoint")
	flag.IntVar(&cfg.MaxSources, "max-sources", cfg.MaxSources, "maximum admitted source streams")
	flag.IntVar(&cfg.MaxSegmentation, "max-segmentation", cfg.MaxSegmentation, "maximum segmentation tasks")
	flag.IntVar(&cfg.MaxGesture, "max-gesture", cfg.MaxGesture, "maximum gesture tasks")
	flag.IntVar(&cfg.QueueSize, "task-queue", cfg.QueueSize, "bounded latest-frame queue per task")
	flag.DurationVar(&cfg.MaxFrameAge, "max-frame-age", cfg.MaxFrameAge, "maximum engine queue age before inference")
	flag.DurationVar(&cfg.TaskTimeout, "task-timeout", cfg.TaskTimeout, "worker send/inference deadline")
	flag.Parse()
	engine, err := videoengine.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer engine.Close()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	srv := grpc.NewServer(grpc.MaxRecvMsgSize(8<<20), grpc.MaxSendMsgSize(8<<20), grpc.MaxConcurrentStreams(uint32(cfg.MaxSources)),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 20 * time.Second, PermitWithoutStream: true}))
	engine.Register(srv)
	httpSrv := &http.Server{Addr: *metrics, Handler: engine, ReadHeaderTimeout: 5 * time.Second}
	metricListener, err := net.Listen("tcp", *metrics)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		if err := httpSrv.Serve(metricListener); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics: %v", err)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		done := make(chan struct{})
		go func() { srv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			srv.Stop()
		}
		_ = httpSrv.Close()
	}()
	log.Printf("video engine listening gRPC=%s metrics=%s segmentation=%s gesture=%s", ln.Addr(), metricListener.Addr(), cfg.SegmentationTarget, cfg.GestureTarget)
	if err := srv.Serve(ln); err != nil {
		log.Fatal(err)
	}
}
