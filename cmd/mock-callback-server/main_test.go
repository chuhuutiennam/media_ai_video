package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// Run the existing main unchanged in a child process, including its flags,
// daemon/final/timeout behavior and stdout JSON consumed by the lab scripts.
func TestCallbackServerProcess(t *testing.T) {
	if os.Getenv("CALLBACK_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("mock-callback-server", flag.ExitOnError)
	main()
	os.Exit(0)
}

type logEntry struct {
	body map[string]json.RawMessage
	err  error
}

func nextLog(t *testing.T, events <-chan logEntry) map[string]json.RawMessage {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok || event.err != nil {
			t.Fatalf("invalid or closed callback log: %v", event.err)
		}
		return event.body
	case <-time.After(5 * time.Second):
		t.Fatal("timeout reading callback log")
		return nil
	}
}

func startCallback(t *testing.T, args ...string) (*exec.Cmd, <-chan logEntry, string, *http.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cmdArgs := append([]string{"-test.run=^TestCallbackServerProcess$", "--", "--port", "0"}, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], cmdArgs...)
	cmd.Env = append(os.Environ(), "CALLBACK_TEST_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(stderr.String())
		}
	})
	events := make(chan logEntry, 64)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var body map[string]json.RawMessage
			err := json.Unmarshal(scanner.Bytes(), &body)
			events <- logEntry{body, err}
		}
		if err := scanner.Err(); err != nil {
			events <- logEntry{err: err}
		}
	}()
	ready := nextLog(t, events)
	var port int
	if string(ready["event"]) != `"ready"` || json.Unmarshal(ready["port"], &port) != nil || port <= 0 {
		t.Fatalf("invalid ready event: %s", ready)
	}
	transport := &http2.Transport{AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
	t.Cleanup(transport.CloseIdleConnections)
	return cmd, events, fmt.Sprintf("http://127.0.0.1:%d", port), &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func TestAudioAndVideoCallbacks(t *testing.T) {
	cmd, events, base, client := startCallback(t, "--expect-final", "0")
	payloads := []string{
		`{"event_type":"asr.transcript.final","is_final":true,"session_id":"audio","text":"xin chào","seq":3,"mediaResources":{"tCore":{"contextId":"ctx","terminationId":"term"}}}`,
		`{"event_type":"video.mask.result","kind":"video_mask","is_final":true,"session_id":"video-taccess","video_mask":{"frame_id":9007199254740993,"rle_counts":[0,2,2],"status":"ok"},"mediaResources":{"tAccess":{"contextId":"ctx","terminationId":"term"}}}`,
	}
	for _, payload := range payloads {
		response, err := client.Post(base+"/callback", "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("HTTP %d", response.StatusCode)
		}
		logged := nextLog(t, events)
		var original map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &original); err != nil {
			t.Fatal(err)
		}
		for key, want := range original {
			if !bytes.Equal(logged[key], want) {
				t.Errorf("%s changed: %s != %s", key, logged[key], want)
			}
		}
		if string(logged["protocol"]) != `"HTTP/2.0"` || string(logged["path"]) != `"/callback"` {
			t.Fatalf("missing HTTP metadata: %s", logged)
		}
	}
	response, err := client.Post(base+"/callback", "application/json", strings.NewReader("{"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatalf("malformed JSON returned HTTP %d", response.StatusCode)
	}
	if event := nextLog(t, events); string(event["event"]) != `"parse_error"` {
		t.Fatalf("missing parse error: %s", event)
	}
	// Both finals were accepted without stopping the daemon; SIGTERM exits cleanly.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if event := nextLog(t, events); string(event["event"]) != `"summary"` {
		t.Fatalf("missing summary: %s", event)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentFinalsExitWithoutPanic(t *testing.T) {
	cmd, events, base, client := startCallback(t, "--expect-final", "1")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := client.Post(base+"/", "application/json", strings.NewReader(
				`{"event_type":"video.mask.result","is_final":true}`))
			// The server exits after its first final; later requests may be refused.
			if err == nil {
				response.Body.Close()
			}
		}()
	}
	wg.Wait()
	for event := range events {
		if event.err != nil {
			t.Fatal(event.err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("concurrent finals: %v", err)
	}
}

func TestCallbackTimeoutExitCode(t *testing.T) {
	cmd, events, _, _ := startCallback(t, "--expect-final", "1", "--timeout", "50ms")
	if event := nextLog(t, events); string(event["event"]) != `"timeout"` {
		t.Fatalf("missing timeout: %s", event)
	}
	if err := cmd.Wait(); err == nil || cmd.ProcessState.ExitCode() != 1 {
		t.Fatalf("expected exit 1, got %v", err)
	}
}
