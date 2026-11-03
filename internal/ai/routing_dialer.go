package ai

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

// GRPCDialFunc mở một bidirectional gRPC stream tới AI worker tại workerAddr.
// Là điểm tích hợp với gRPC transport layer thực; được inject vào RoutingDialer.
type GRPCDialFunc func(ctx context.Context, workerAddr, sessionID, streamID, language, task string) (StreamClient, error)

// RoutingDialer implements ai.Dialer bằng cách chọn AI worker phù hợp từ
// WorkerRegistry rồi mở stream qua GRPCDialFunc.
//
// Routing 2 bước:
//  1. Filter: loại worker không hỗ trợ language/task hoặc đã đạt giới hạn stream.
//  2. Select: chọn worker có loadScore thấp nhất (stream ratio + GPU load).
type RoutingDialer struct {
	registry *WorkerRegistry
	dialFn   GRPCDialFunc
}

// NewRoutingDialer tạo RoutingDialer với registry và dial function cho trước.
func NewRoutingDialer(registry *WorkerRegistry, dialFn GRPCDialFunc) *RoutingDialer {
	return &RoutingDialer{registry: registry, dialFn: dialFn}
}

// Dial chọn AI worker phù hợp và mở stream tới worker đó.
// Trả về ErrNoWorkerAvailable nếu không có worker nào đáp ứng yêu cầu.
func (d *RoutingDialer) Dial(ctx context.Context, sessionID, streamID, language, task string) (StreamClient, error) {
	worker, err := d.registry.Select(language, task)
	if err != nil {
		zap.L().Debug("ai: no worker available", zap.String("session_id", sessionID), zap.String("language", language), zap.String("task", task), zap.Error(err))
		return nil, fmt.Errorf("ai: route %s/%s: %w", language, task, err)
	}
	zap.L().Debug("ai: dialing worker", zap.String("session_id", sessionID), zap.String("worker_id", worker.ID), zap.String("worker_addr", worker.Addr), zap.String("language", language), zap.String("task", task))
	client, err := d.dialFn(ctx, worker.Addr, sessionID, streamID, language, task)
	if err != nil {
		zap.L().Debug("ai: dial failed", zap.String("session_id", sessionID), zap.String("worker_id", worker.ID), zap.String("worker_addr", worker.Addr), zap.Error(err))
		return nil, fmt.Errorf("ai: dial worker %s (%s): %w", worker.ID, worker.Addr, err)
	}
	return client, nil
}
