package session

import (
	"testing"
	"time"
)

// Tác vụ nền gắn với session cũ (vd CALL_CTRL lỗi) không được đóng session mới dùng lại cùng ID.
func TestCloseSessionIgnoresReusedID(t *testing.T) {
	mgr := NewManager(ManagerConfig{MaxSessions: 2, PacketQueueSize: 1, AudioQueueSize: 1, ResultQueueSize: 1,
		IdleTimeout: time.Minute, GCInterval: time.Minute})
	old, err := mgr.Create(SessionConfig{ID: "call-tcore"})
	if err != nil {
		t.Fatal(err)
	}
	if !mgr.Close("call-tcore") {
		t.Fatal("close old")
	}
	fresh, err := mgr.Create(SessionConfig{ID: "call-tcore"})
	if err != nil {
		t.Fatal(err)
	}
	if mgr.CloseSession(old) {
		t.Fatal("CloseSession(old) closed the new session with the same ID")
	}
	if got, ok := mgr.Get("call-tcore"); !ok || got != fresh || fresh.Ctx.Err() != nil {
		t.Fatal("new session must stay open")
	}
	if !mgr.CloseSession(fresh) || fresh.Ctx.Err() == nil {
		t.Fatal("CloseSession(fresh) must close it")
	}
	if mgr.CloseSession(fresh) {
		t.Fatal("second CloseSession must report false")
	}
}
