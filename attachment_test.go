// SPDX-FileCopyrightText: The go-mail Authors
//
// SPDX-License-Identifier: MIT

package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// snapshotLockedBuffer is a concurrency-safe buffer capturing the fake SMTP server
// conversation for assertions.
type snapshotLockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *snapshotLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *snapshotLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startSnapshotTestServer starts the simple fake SMTP server on a free test port and returns
// the port and the buffer echoing the full SMTP conversation.
func startSnapshotTestServer(t *testing.T) (int, *snapshotLockedBuffer) {
	t.Helper()
	ctx := t.Context()
	PortAdder.Add(1)
	serverPort := int(TestServerPortBase + PortAdder.Load())
	featureSet := "250-AUTH PLAIN\r\n250-8BITMIME\r\n250-DSN\r\n250 SMTPUTF8"
	buffer := &snapshotLockedBuffer{}
	go func() {
		if err := simpleSMTPServer(ctx, t, &serverProps{
			FeatureSet: featureSet, ListenPort: serverPort, EchoBuffer: buffer,
		}); err != nil {
			t.Errorf("failed to start test server: %s", err)
			return
		}
	}()
	time.Sleep(time.Millisecond * 50)
	return serverPort, buffer
}

// newSnapshotTestClient returns a Client connected to the fake SMTP server on the given port.
func newSnapshotTestClient(t *testing.T, serverPort int) *Client {
	t.Helper()
	ctxDial, cancelDial := context.WithTimeout(t.Context(), time.Second*5)
	t.Cleanup(cancelDial)
	client, err := NewClient(DefaultHost, WithPort(serverPort), WithTLSPolicy(NoTLS))
	if err != nil {
		t.Fatalf("failed to create new client: %s", err)
	}
	if err = client.DialWithContext(ctxDial); err != nil {
		t.Fatalf("failed to connect to test server: %s", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// newSnapshotTestMsg returns a deterministic Msg with the given snapshot attached.
func newSnapshotTestMsg(t *testing.T, snapshot *AttachmentSnapshot) *Msg {
	t.Helper()
	msg := NewMsg()
	msg.SetBoundary("snapshot-test-boundary")
	msg.SetMessageIDWithValue("snapshot-test@domain.tld")
	msg.SetDateWithValue(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if err := msg.From(TestSenderValid); err != nil {
		t.Fatalf("failed to set sender: %s", err)
	}
	if err := msg.To(TestRcptValid); err != nil {
		t.Fatalf("failed to set recipient: %s", err)
	}
	msg.Subject("snapshot test")
	msg.SetBodyString(TypeTextPlain, "test body")
	if err := msg.AttachSnapshot(snapshot); err != nil {
		t.Fatalf("failed to attach snapshot: %s", err)
	}
	return msg
}

func TestSnapshotFactory_FromReader_deterministic(t *testing.T) {
	content := []byte("small attachment content for deterministic builds")
	factory := NewSnapshotFactory(0)

	snapshot1, err := factory.FromReader("report.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("failed to build first snapshot: %s", err)
	}
	snapshot2, err := factory.FromReader("report.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("failed to build second snapshot: %s", err)
	}

	if snapshot1.Name() != snapshot2.Name() {
		t.Errorf("snapshot names differ: %q vs %q", snapshot1.Name(), snapshot2.Name())
	}
	if snapshot1.Size() != snapshot2.Size() {
		t.Errorf("snapshot sizes differ: %d vs %d", snapshot1.Size(), snapshot2.Size())
	}
	sum1, ok1 := snapshot1.Sum()
	sum2, ok2 := snapshot2.Sum()
	if !ok1 || !ok2 {
		t.Fatal("expected content hashes to be known after build")
	}
	if sum1 != sum2 {
		t.Errorf("snapshot hashes differ: %x vs %x", sum1, sum2)
	}

	render := func(snapshot *AttachmentSnapshot) string {
		msg := newSnapshotTestMsg(t, snapshot)
		var buf bytes.Buffer
		if _, err := msg.WriteTo(&buf); err != nil {
			t.Fatalf("failed to render message: %s", err)
		}
		return buf.String()
	}
	first := render(snapshot1)
	second := render(snapshot2)
	if first != second {
		t.Errorf("two builds of the same small attachment rendered differently:\n%s\n---\n%s",
			first, second)
	}
	// A cached snapshot is replayable: rendering the same snapshot twice is identical.
	if again := render(snapshot1); again != first {
		t.Error("re-rendering the same cached snapshot produced different output")
	}
}

func TestSnapshotFactory_FromFile_modifiedRejected(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(filePath, []byte("original content"), 0o600); err != nil {
		t.Fatalf("failed to write test file: %s", err)
	}
	factory := NewSnapshotFactory(0)
	snapshot, err := factory.FromFile(filePath)
	if err != nil {
		t.Fatalf("failed to build file snapshot: %s", err)
	}
	if snapshot.Name() != "data.txt" {
		t.Errorf("expected snapshot name %q, got %q", "data.txt", snapshot.Name())
	}
	if !snapshot.Replayable() {
		t.Error("file-backed snapshot must be replayable")
	}

	t.Run("unmodified file passes verification", func(t *testing.T) {
		msg := newSnapshotTestMsg(t, snapshot)
		if err := msg.verifyAttachmentSnapshots(); err != nil {
			t.Errorf("expected verification to pass, got: %s", err)
		}
	})

	t.Run("size change is rejected", func(t *testing.T) {
		if err := os.WriteFile(filePath, []byte("tampered content!!"), 0o600); err != nil {
			t.Fatalf("failed to modify test file: %s", err)
		}
		msg := newSnapshotTestMsg(t, snapshot)
		if err := msg.verifyAttachmentSnapshots(); !errors.Is(err, ErrSnapshotModified) {
			t.Errorf("expected ErrSnapshotModified, got: %v", err)
		}
	})

	t.Run("content-only change with restored modtime is rejected", func(t *testing.T) {
		// Same length as "original content" and restored modification time: only the
		// content hash can catch this modification.
		if err := os.WriteFile(filePath, []byte("TAMPERED CONTENT"), 0o600); err != nil {
			t.Fatalf("failed to modify test file: %s", err)
		}
		if err := os.Chtimes(filePath, snapshot.modTime, snapshot.modTime); err != nil {
			t.Fatalf("failed to restore modtime: %s", err)
		}
		msg := newSnapshotTestMsg(t, snapshot)
		if err := msg.verifyAttachmentSnapshots(); !errors.Is(err, ErrSnapshotModified) {
			t.Errorf("expected ErrSnapshotModified, got: %v", err)
		}
	})

	t.Run("send of modified snapshot hands nothing to SMTP", func(t *testing.T) {
		if err := os.WriteFile(filePath, []byte("completely new content on disk"), 0o600); err != nil {
			t.Fatalf("failed to modify test file: %s", err)
		}
		serverPort, buffer := startSnapshotTestServer(t)
		client := newSnapshotTestClient(t, serverPort)

		msg := newSnapshotTestMsg(t, snapshot)
		err := client.Send(msg)
		if err == nil {
			t.Fatal("expected send to fail for modified snapshot")
		}
		if !errors.Is(err, ErrSnapshotModified) {
			t.Errorf("expected ErrSnapshotModified, got: %v", err)
		}
		var sendErr *SendError
		if !errors.As(err, &sendErr) {
			t.Fatalf("expected SendError, got: %T", err)
		}
		if sendErr.Reason != ErrSnapshotCheck {
			t.Errorf("expected reason ErrSnapshotCheck, got: %s", sendErr.Reason)
		}
		if sendErr.IsTemp() {
			t.Error("snapshot check failure must not be a temporary error")
		}
		if conversation := buffer.String(); strings.Contains(conversation, "MAIL FROM") {
			t.Errorf("no mail data must be handed to the SMTP server, got conversation:\n%s",
				conversation)
		}
	})

	t.Run("explicit rebuild through factory sends new content", func(t *testing.T) {
		rebuilt, err := factory.FromFile(filePath)
		if err != nil {
			t.Fatalf("failed to rebuild snapshot: %s", err)
		}
		serverPort, buffer := startSnapshotTestServer(t)
		client := newSnapshotTestClient(t, serverPort)
		msg := newSnapshotTestMsg(t, rebuilt)
		if err := client.Send(msg); err != nil {
			t.Fatalf("expected send of rebuilt snapshot to succeed, got: %s", err)
		}
		want := base64.StdEncoding.EncodeToString([]byte("completely new content on disk"))
		if !strings.Contains(buffer.String(), want) {
			t.Errorf("expected SMTP conversation to contain new content %q", want)
		}
	})
}

func TestSnapshotFactory_FromReaderOnce_notReplayable(t *testing.T) {
	serverPort, buffer := startSnapshotTestServer(t)
	client := newSnapshotTestClient(t, serverPort)

	factory := NewSnapshotFactory(0)
	snapshot, err := factory.FromReaderOnce("once.txt", strings.NewReader("single use content"))
	if err != nil {
		t.Fatalf("failed to build single-use snapshot: %s", err)
	}
	if snapshot.Replayable() {
		t.Error("single-use snapshot must not be replayable")
	}
	if snapshot.Consumed() {
		t.Error("single-use snapshot must not be consumed before first use")
	}
	if size := snapshot.Size(); size != -1 {
		t.Errorf("expected unknown size -1 before consumption, got: %d", size)
	}

	msg := newSnapshotTestMsg(t, snapshot)
	if err := client.Send(msg); err != nil {
		t.Fatalf("expected first send to succeed, got: %s", err)
	}
	want := base64.StdEncoding.EncodeToString([]byte("single use content"))
	if !strings.Contains(buffer.String(), want) {
		t.Errorf("expected SMTP conversation to contain content %q", want)
	}
	if !snapshot.Consumed() {
		t.Error("single-use snapshot must be consumed after first send")
	}

	// A send retry must not silently reuse the consumed reader: it has to fail
	// explicitly before anything is handed to the SMTP server.
	buffer.mu.Lock()
	buffer.buf.Reset()
	buffer.mu.Unlock()
	err = client.Send(msg)
	if err == nil {
		t.Fatal("expected send retry of consumed snapshot to fail")
	}
	if !errors.Is(err, ErrSnapshotConsumed) {
		t.Errorf("expected ErrSnapshotConsumed, got: %v", err)
	}
	var sendErr *SendError
	if !errors.As(err, &sendErr) {
		t.Fatalf("expected SendError, got: %T", err)
	}
	if sendErr.Reason != ErrSnapshotCheck {
		t.Errorf("expected reason ErrSnapshotCheck, got: %s", sendErr.Reason)
	}
	if conversation := buffer.String(); strings.Contains(conversation, "MAIL FROM") {
		t.Errorf("consumed snapshot retry must not reach the SMTP server, got:\n%s", conversation)
	}

	// Rendering the message directly must fail explicitly as well.
	if _, err := msg.WriteTo(io.Discard); !errors.Is(err, ErrSnapshotConsumed) {
		t.Errorf("expected ErrSnapshotConsumed from WriteTo, got: %v", err)
	}
}

func TestSnapshotFactory_cacheLimit(t *testing.T) {
	factory := NewSnapshotFactory(10)
	if factory.CacheLimit() != 10 {
		t.Errorf("expected cache limit 10, got: %d", factory.CacheLimit())
	}
	if _, err := factory.FromReader("a.txt", strings.NewReader("12345")); err != nil {
		t.Fatalf("expected first snapshot to fit the cache, got: %s", err)
	}
	if used := factory.CacheUsed(); used != 5 {
		t.Errorf("expected 5 cache bytes used, got: %d", used)
	}
	// 6 more bytes would exceed the total budget of 10 bytes.
	if _, err := factory.FromReader("b.txt", strings.NewReader("123456")); !errors.Is(err, ErrSnapshotCacheExhausted) {
		t.Errorf("expected ErrSnapshotCacheExhausted, got: %v", err)
	}
	if used := factory.CacheUsed(); used != 5 {
		t.Errorf("failed build must not consume cache budget, used: %d", used)
	}
	// Exactly filling the budget is allowed.
	if _, err := factory.FromReader("c.txt", strings.NewReader("12345")); err != nil {
		t.Fatalf("expected exact-fit snapshot to succeed, got: %s", err)
	}
	if _, err := factory.FromReader("d.txt", strings.NewReader("1")); !errors.Is(err, ErrSnapshotCacheExhausted) {
		t.Errorf("expected ErrSnapshotCacheExhausted beyond the limit, got: %v", err)
	}
	// File-backed and single-use snapshots do not consume the cache budget.
	dir := t.TempDir()
	filePath := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(filePath, []byte("more than ten bytes of content"), 0o600); err != nil {
		t.Fatalf("failed to write test file: %s", err)
	}
	if _, err := factory.FromFile(filePath); err != nil {
		t.Errorf("file-backed snapshot must not be limited by cache budget: %s", err)
	}
	if _, err := factory.FromReaderOnce("e.txt", strings.NewReader("more than ten bytes")); err != nil {
		t.Errorf("single-use snapshot must not be limited by cache budget: %s", err)
	}
	if used := factory.CacheUsed(); used != 10 {
		t.Errorf("expected 10 cache bytes used, got: %d", used)
	}
}

func TestSnapshotFactory_defaultCacheLimit(t *testing.T) {
	factory := NewSnapshotFactory(0)
	if factory.CacheLimit() != DefaultSnapshotCacheLimit {
		t.Errorf("expected default cache limit %d, got: %d", DefaultSnapshotCacheLimit,
			factory.CacheLimit())
	}
}

func TestAttachmentSnapshot_filenameIsNotAPath(t *testing.T) {
	backslash := string(rune(92))
	tests := []struct {
		name     string
		given    string
		wantName string
	}{
		{"unix path", "/home/user/secret.pdf", "secret.pdf"},
		{"windows path", "C:" + backslash + "Users" + backslash + "user" + backslash + "secret.pdf", "secret.pdf"},
		{"relative traversal", ".." + backslash + ".." + backslash + "secret.pdf", "secret.pdf"},
		{"plain name", "secret.pdf", "secret.pdf"},
		{"empty name", "", "attachment"},
	}
	factory := NewSnapshotFactory(0)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := factory.FromReader(tc.given, strings.NewReader("x"))
			if err != nil {
				t.Fatalf("failed to build snapshot: %s", err)
			}
			if snapshot.Name() != tc.wantName {
				t.Errorf("expected snapshot name %q, got %q", tc.wantName, snapshot.Name())
			}
		})
	}
}

func TestAttachmentSnapshot_filenameMimeEncoded(t *testing.T) {
	backslash := string(rune(92))
	factory := NewSnapshotFactory(0)
	snapshot, err := factory.FromReader("dir"+backslash+"报表 final.pdf",
		strings.NewReader("content"))
	if err != nil {
		t.Fatalf("failed to build snapshot: %s", err)
	}
	if snapshot.Name() != "报表 final.pdf" {
		t.Errorf("expected base name only, got %q", snapshot.Name())
	}

	msg := newSnapshotTestMsg(t, snapshot)
	var buf bytes.Buffer
	if _, err := msg.WriteTo(&buf); err != nil {
		t.Fatalf("failed to render message: %s", err)
	}
	rendered := buf.String()
	// The non-ASCII filename must be MIME encoded-word encoded in the attachment headers.
	if !strings.Contains(rendered, "=?UTF-8?") {
		t.Errorf("expected MIME encoded filename in output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "filename=") {
		t.Errorf("expected Content-Disposition filename in output:\n%s", rendered)
	}
	// The name must never appear as a path: no directory component or separator of the
	// original input may leak into the message.
	if strings.Contains(rendered, "dir"+backslash) || strings.Contains(rendered, "dir/") {
		t.Errorf("filename must not be rendered as a path:\n%s", rendered)
	}
}

func TestMsg_AttachSnapshot_nil(t *testing.T) {
	msg := NewMsg()
	if err := msg.AttachSnapshot(nil); !errors.Is(err, ErrSnapshotNil) {
		t.Errorf("expected ErrSnapshotNil, got: %v", err)
	}
}

func TestClient_Send_snapshotHappyPath(t *testing.T) {
	serverPort, buffer := startSnapshotTestServer(t)
	client := newSnapshotTestClient(t, serverPort)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "fromdisk.txt")
	if err := os.WriteFile(filePath, []byte("content from disk file"), 0o600); err != nil {
		t.Fatalf("failed to write test file: %s", err)
	}

	factory := NewSnapshotFactory(0)
	cached, err := factory.FromReader("memory.txt", strings.NewReader("content from memory"))
	if err != nil {
		t.Fatalf("failed to build reader snapshot: %s", err)
	}
	fromFile, err := factory.FromFile(filePath)
	if err != nil {
		t.Fatalf("failed to build file snapshot: %s", err)
	}

	msg := NewMsg()
	if err := msg.From(TestSenderValid); err != nil {
		t.Fatalf("failed to set sender: %s", err)
	}
	if err := msg.To(TestRcptValid); err != nil {
		t.Fatalf("failed to set recipient: %s", err)
	}
	msg.Subject("snapshot happy path")
	msg.SetBodyString(TypeTextPlain, "body")
	if err := msg.AttachSnapshot(cached); err != nil {
		t.Fatalf("failed to attach cached snapshot: %s", err)
	}
	if err := msg.AttachSnapshot(fromFile); err != nil {
		t.Fatalf("failed to attach file snapshot: %s", err)
	}

	// Send twice (simulating a retry): replayable snapshots must deliver identical
	// content both times.
	for i := 0; i < 2; i++ {
		if err := client.Send(msg); err != nil {
			t.Fatalf("send %d failed: %s", i+1, err)
		}
	}
	conversation := buffer.String()
	for _, content := range []string{"content from memory", "content from disk file"} {
		want := base64.StdEncoding.EncodeToString([]byte(content))
		if count := strings.Count(conversation, want); count != 2 {
			t.Errorf("expected content %q (base64 %q) twice in conversation, found %d",
				content, want, count)
		}
	}
}

// ExampleMsg_AttachSnapshot demonstrates how to build an explicit attachment snapshot and
// attach it to a Msg. The snapshot binds the attachment name and content in a single build
// step; the send path verifies it before anything is handed to the SMTP server.
func ExampleMsg_AttachSnapshot() {
	// A SnapshotFactory bounds the total in-memory cache used by reader snapshots.
	factory := NewSnapshotFactory(DefaultSnapshotCacheLimit)

	// Bind name and content in a single build step. The reader is consumed once, at
	// build time; sends and retries replay the cached copy.
	snapshot, err := factory.FromReader("report.txt", strings.NewReader("report content"))
	if err != nil {
		return
	}

	msg := NewMsg()
	_ = msg.From("sender@example.com")
	_ = msg.To("recipient@example.com")
	msg.Subject("report")
	msg.SetBodyString(TypeTextPlain, "see attachment")
	if err := msg.AttachSnapshot(snapshot); err != nil {
		return
	}
}
