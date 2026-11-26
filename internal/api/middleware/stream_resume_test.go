package middleware

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/proxy"
)

// testClient bounds every stream read so a regression fails fast instead of
// hanging until the package timeout.
var testClient = &http.Client{Timeout: 30 * time.Second}

// keyAuth is a stand-in for the real AuthMiddleware: it binds the principal that
// the gateway would have authenticated to the userApiKey context key.
func keyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader("X-Test-Key")
		if key == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing key"})
			return
		}
		c.Set("userApiKey", key)
		c.Next()
	}
}

// resumeGate is the test's handle on a producer that pauses mid-generation.
type resumeGate struct {
	release chan struct{}
	once    sync.Once
}

// Release lets the paused producer continue. It is idempotent and safe to call
// from cleanup, so a failing assertion never strands the producer and turns a
// fast failure into a shutdown hang.
func (g *resumeGate) Release() {
	if g == nil {
		return
	}
	g.once.Do(func() { close(g.release) })
}

// newResumeServer wires a route that emits total SSE frames tagged with their
// index. With pauseAfter > 0 the producer pauses after that frame until the
// returned gate is released, which lets a test drop the consumer while the
// generation is still in flight.
func newResumeServer(t *testing.T, total, pauseAfter int) (*httptest.Server, *proxy.Registry, *resumeGate) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	registry := proxy.NewRegistry(proxy.RegistryOptions{})
	var gate *resumeGate
	if pauseAfter > 0 {
		gate = &resumeGate{release: make(chan struct{})}
	}
	engine := gin.New()
	engine.POST("/v1/chat/completions", keyAuth(), StreamResume(registry), func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Status(http.StatusOK)
		c.Writer.Flush()
		// Mirror the real SSE handlers, which stop producing as soon as their
		// request context is done. Resumable requests must therefore run on a
		// context that survives the client's disconnect.
		ctx := c.Request.Context()
		for i := 1; i <= total; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			_, _ = fmt.Fprintf(c.Writer, "data: {\"i\":%d}\n\n", i)
			c.Writer.Flush()
			if gate != nil && i == pauseAfter {
				select {
				case <-gate.release:
				case <-ctx.Done():
					return
				}
			}
		}
	})
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	t.Cleanup(registry.Close)
	// Registered last so it runs first: a producer still blocked on the gate
	// would keep httptest.Server.Close waiting forever.
	t.Cleanup(gate.Release)
	return server, registry, gate
}

type sseEvent struct {
	id      string
	payload string
}

// readEvents reads n SSE events from body, returning them along with the stream
// identifier header.
func readEvents(t *testing.T, body *bufio.Reader, n int) []sseEvent {
	t.Helper()
	var events []sseEvent
	var current sseEvent
	for len(events) < n {
		line, err := body.ReadString('\n')
		if err != nil {
			t.Fatalf("read event %d: %v", len(events)+1, err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "id:"):
			current.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			current.payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if current.payload != "" {
				events = append(events, current)
				current = sseEvent{}
			}
		}
	}
	return events
}

// TestStreamResumeAC1ReconnectFromBreakpoint is acceptance criterion 1: after
// the consumer loses its connection mid-stream, a reconnect carrying
// Last-Event-ID continues exactly from the next event, with no gap and no
// duplicate, and the rejoined sequence equals the original.
func TestStreamResumeAC1ReconnectFromBreakpoint(t *testing.T) {
	const total = 600
	const breakpoint = 500
	server, registry, gate := newResumeServer(t, total, breakpoint)

	// First consumer: opt in, read 500 events, then drop the connection.
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Test-Key", "sk-consumer")
	req.Header.Set(HeaderStreamResume, "1")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	streamID := resp.Header.Get(HeaderStreamID)
	if streamID == "" {
		t.Fatal("first response must carry X-Stream-Id")
	}
	first := readEvents(t, bufio.NewReader(resp.Body), breakpoint)
	if len(first) != breakpoint {
		t.Fatalf("read %d events, want %d", len(first), breakpoint)
	}
	if first[breakpoint-1].id != strconv.Itoa(breakpoint) {
		t.Fatalf("event %d id = %q, want %q", breakpoint, first[breakpoint-1].id, strconv.Itoa(breakpoint))
	}
	// The consumer drops here. The producer is paused, so it can only complete
	// the remaining frames if it survived the disconnect.
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("close body: %v", errClose)
	}
	waitProducerDetached(t, registry, streamID)
	gate.Release()
	waitFramesProduced(t, registry, streamID, total)

	// Second consumer: reconnect from the breakpoint.
	req2, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build resume request: %v", err)
	}
	req2.Header.Set("X-Test-Key", "sk-consumer")
	req2.Header.Set(HeaderStreamID, streamID)
	req2.Header.Set(HeaderLastEventID, strconv.Itoa(breakpoint))
	resp2, err := testClient.Do(req2)
	if err != nil {
		t.Fatalf("resume request: %v", err)
	}
	defer func() {
		if errClose := resp2.Body.Close(); errClose != nil {
			t.Errorf("close resume body: %v", errClose)
		}
	}()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d, want 200", resp2.StatusCode)
	}
	rest := readEvents(t, bufio.NewReader(resp2.Body), total-breakpoint)
	if len(rest) != total-breakpoint {
		t.Fatalf("resumed %d events, want %d", len(rest), total-breakpoint)
	}
	for i, ev := range rest {
		wantSeq := breakpoint + 1 + i
		if ev.id != strconv.Itoa(wantSeq) {
			t.Fatalf("resumed event %d id = %q, want %q", i, ev.id, strconv.Itoa(wantSeq))
		}
		if ev.payload != fmt.Sprintf("{\"i\":%d}", wantSeq) {
			t.Fatalf("resumed event %d payload = %q, want i=%d", i, ev.payload, wantSeq)
		}
	}
	// Rejoined stream is the original, whole and in order.
	combined := append(first, rest...)
	if len(combined) != total {
		t.Fatalf("combined = %d events, want %d", len(combined), total)
	}
	for i := 1; i <= total; i++ {
		if combined[i-1].payload != fmt.Sprintf("{\"i\":%d}", i) {
			t.Fatalf("combined event %d = %q, want i=%d", i, combined[i-1].payload, i)
		}
	}
}

// TestStreamResumeAC2ForeignKeyForbidden is acceptance criterion 2: a resume
// attempt with a different API key is rejected with 403 and leaks no stream
// content.
func TestStreamResumeAC2ForeignKeyForbidden(t *testing.T) {
	const total = 50
	server, registry, _ := newResumeServer(t, total, 0)

	resp, streamID := openResumable(t, server, "sk-owner")
	_ = readEvents(t, bufio.NewReader(resp.Body), 5)
	_ = registry // session retained while producer is still held

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Test-Key", "sk-intruder")
	req.Header.Set(HeaderStreamID, streamID)
	req.Header.Set(HeaderLastEventID, "5")
	intruder, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("intruder request: %v", err)
	}
	defer func() {
		if errClose := intruder.Body.Close(); errClose != nil {
			t.Errorf("close intruder body: %v", errClose)
		}
	}()
	if intruder.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", intruder.StatusCode)
	}
	body := readAll(t, intruder)
	if strings.Contains(body, "\"i\":") || strings.Contains(body, "data:") {
		t.Fatalf("403 body leaked stream content: %q", body)
	}
	if !strings.Contains(body, "stream_forbidden") {
		t.Fatalf("403 body = %q, want a stream_forbidden error", body)
	}

	// The owner is still able to resume.
	req2, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build owner request: %v", err)
	}
	req2.Header.Set("X-Test-Key", "sk-owner")
	req2.Header.Set(HeaderStreamID, streamID)
	req2.Header.Set(HeaderLastEventID, "5")
	owner, err := testClient.Do(req2)
	if err != nil {
		t.Fatalf("owner resume: %v", err)
	}
	defer func() {
		if errClose := owner.Body.Close(); errClose != nil {
			t.Errorf("close owner body: %v", errClose)
		}
	}()
	if owner.StatusCode != http.StatusOK {
		t.Fatalf("owner resume status = %d, want 200", owner.StatusCode)
	}
	_ = readAll(t, owner)
}

func TestStreamResumeUnknownStream(t *testing.T) {
	server, _, _ := newResumeServer(t, 10, 0)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Test-Key", "sk-a")
	req.Header.Set(HeaderStreamID, "does-not-exist")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Errorf("close body: %v", errClose)
		}
	}()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410", resp.StatusCode)
	}
}

func TestStreamResumeWithoutOptInIsUnchanged(t *testing.T) {
	server, registry, _ := newResumeServer(t, 10, 0)
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Errorf("close body: %v", errClose)
		}
	}()
	if got := resp.Header.Get(HeaderStreamID); got != "" {
		t.Fatalf("X-Stream-Id = %q on a non-opted-in request, want empty", got)
	}
	body := readAll(t, resp)
	if strings.Contains(body, "id:") {
		t.Fatalf("non-opted-in stream must not be re-tagged: %q", body)
	}
	if registry.Len() != 0 {
		t.Fatalf("a non-opted-in request must not retain a session, Len = %d", registry.Len())
	}
}

// A request that opted in but produced a non-streaming response (for example a
// validation error) must not leave a retained session behind.
func TestStreamResumeNonStreamingResponseReleasesSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := proxy.NewRegistry(proxy.RegistryOptions{})
	t.Cleanup(registry.Close)

	engine := gin.New()
	engine.POST("/v1/chat/completions", keyAuth(), StreamResume(registry), func(c *gin.Context) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad request"})
	})
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Test-Key", "sk-a")
	req.Header.Set(HeaderStreamResume, "1")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Errorf("close body: %v", errClose)
		}
	}()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if registry.Len() != 0 {
		t.Fatalf("non-streaming response retained a session, Len = %d", registry.Len())
	}
}

// An event the upstream already tagged keeps exactly one id field, so the
// resume sequence stays authoritative.
func TestStreamResumeRetagsUpstreamEventIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := proxy.NewRegistry(proxy.RegistryOptions{})
	t.Cleanup(registry.Close)

	engine := gin.New()
	engine.POST("/v1/chat/completions", keyAuth(), StreamResume(registry), func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		c.Writer.Flush()
		_, _ = fmt.Fprint(c.Writer, "id: upstream-777\ndata: {\"i\":1}\n\n")
		c.Writer.Flush()
	})
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)

	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", nil)
	if err != nil {
		t.Fatalf("request without key: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected auth to gate the route, got %d", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Test-Key", "sk-a")
	req.Header.Set(HeaderStreamResume, "1")
	resp2, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() {
		if errClose := resp2.Body.Close(); errClose != nil {
			t.Errorf("close body: %v", errClose)
		}
	}()
	body := readAll(t, resp2)
	if strings.Contains(body, "upstream-777") {
		t.Fatalf("upstream id was not replaced: %q", body)
	}
	if !strings.Contains(body, "id: 1\n") {
		t.Fatalf("gateway id missing: %q", body)
	}
}

// openResumable opens a resumable stream and returns the response and its id.
func openResumable(t *testing.T, server *httptest.Server, key string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Test-Key", key)
	req.Header.Set(HeaderStreamResume, "1")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("open request: %v", err)
	}
	streamID := resp.Header.Get(HeaderStreamID)
	if streamID == "" {
		_ = resp.Body.Close()
		t.Fatal("response must carry X-Stream-Id")
	}
	return resp, streamID
}

// waitFramesProduced blocks until the session recorded total frames, proving the
// producer survived the client disconnect and completed the generation.
func waitFramesProduced(t *testing.T, registry *proxy.Registry, streamID string, total int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, ok := registry.Lookup(streamID)
		if ok && session.LastSeq() >= int64(total) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("producer did not survive the client disconnect and finish")
}

// waitProducerDetached blocks until the middleware observed the client's
// disconnect (dropping its hold on the session). Until then the producer may not
// have survived the drop at all.
func waitProducerDetached(t *testing.T, registry *proxy.Registry, streamID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, ok := registry.Lookup(streamID)
		if ok && !session.Held() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("middleware never observed the client disconnect")
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			return sb.String()
		}
	}
}

// TestStreamResumeConcurrentConsumersKeepSequence ensures the tee does not
// duplicate or drop events when a live reader and a resuming reader attach at
// the same time.
func TestStreamResumeConcurrentConsumersKeepSequence(t *testing.T) {
	const total = 300
	server, _, gate := newResumeServer(t, total, total/2)

	resp, streamID := openResumable(t, server, "sk-c")
	var wg sync.WaitGroup
	results := make([][]sseEvent, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0] = readEvents(t, bufio.NewReader(resp.Body), total)
	}()
	// Attach a second reader from the beginning while the first is live.
	go func() {
		defer wg.Done()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
		if err != nil {
			t.Errorf("build request: %v", err)
			return
		}
		req.Header.Set("X-Test-Key", "sk-c")
		req.Header.Set(HeaderStreamID, streamID)
		req.Header.Set(HeaderLastEventID, "0")
		resp2, err := testClient.Do(req)
		if err != nil {
			t.Errorf("second reader: %v", err)
			return
		}
		defer func() {
			if errClose := resp2.Body.Close(); errClose != nil {
				t.Errorf("close second body: %v", errClose)
			}
		}()
		results[1] = readEvents(t, bufio.NewReader(resp2.Body), total)
	}()
	gate.Release()
	wg.Wait()
	if errClose := resp.Body.Close(); errClose != nil {
		t.Errorf("close body: %v", errClose)
	}
	for idx, events := range results {
		if len(events) != total {
			t.Fatalf("reader %d got %d events, want %d", idx, len(events), total)
		}
		for i, ev := range events {
			want := strconv.Itoa(i + 1)
			if ev.id != want {
				t.Fatalf("reader %d event %d id = %q, want %q", idx, i, ev.id, want)
			}
		}
	}
}

// TestStreamResumeContextCancelsWithoutSession ensures a plain (non-opted-in)
// request keeps the original cancellation semantics.
func TestStreamResumeContextCancelsWithoutSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := proxy.NewRegistry(proxy.RegistryOptions{})
	t.Cleanup(registry.Close)
	observed := make(chan struct{})
	engine := gin.New()
	engine.POST("/v1/chat/completions", keyAuth(), StreamResume(registry), func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		c.Writer.Flush()
		<-c.Request.Context().Done()
		close(observed)
	})
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Test-Key", "sk-a")
	go func() {
		resp, errDo := testClient.Do(req)
		if errDo == nil {
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("non-resumable request did not observe client cancellation")
	}
}
