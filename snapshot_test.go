// SPDX-FileCopyrightText: The go-mail Authors
//
// SPDX-License-Identifier: MIT

package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// renderMessage renders the given Msg into a string, simulating one full message
// generation round.
func renderMessage(t *testing.T, message *Msg) string {
	t.Helper()
	buffer := bytes.Buffer{}
	if _, err := message.WriteTo(&buffer); err != nil {
		t.Fatalf("failed to render message: %s", err)
	}
	return buffer.String()
}

// writeSnapshot builds a File from the given AttachmentSnapshot and writes its content
// into a buffer, simulating one message generation round.
func writeSnapshot(t *testing.T, snapshot *AttachmentSnapshot) []byte {
	t.Helper()
	file, err := snapshot.File()
	if err != nil {
		t.Fatalf("failed to build file from snapshot: %s", err)
	}
	buffer := bytes.Buffer{}
	if _, err = file.Writer(&buffer); err != nil {
		t.Fatalf("failed to write snapshot content: %s", err)
	}
	return buffer.Bytes()
}

func TestSnapshotter_SnapshotFile_TwiceIdentical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.txt")
	content := []byte("small attachment content\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("failed to write test file: %s", err)
	}

	snapshot, err := NewSnapshotter().SnapshotFile(path)
	if err != nil {
		t.Fatalf("failed to snapshot file: %s", err)
	}
	if snapshot.Name() != "report.txt" {
		t.Errorf("expected snapshot name %q, got %q", "report.txt", snapshot.Name())
	}
	if snapshot.Size() != int64(len(content)) {
		t.Errorf("expected snapshot size %d, got %d", len(content), snapshot.Size())
	}

	first := writeSnapshot(t, snapshot)
	second := writeSnapshot(t, snapshot)
	if !bytes.Equal(first, second) {
		t.Errorf("expected two builds of the same snapshot to be identical")
	}
	if !bytes.Equal(first, content) {
		t.Errorf("expected snapshot content %q, got %q", content, first)
	}

	// Two full message generation rounds using the same snapshot must be identical, too.
	message := NewMsg()
	if err := message.From(TestSenderValid); err != nil {
		t.Fatalf("failed to set FROM: %s", err)
	}
	if err := message.To(TestRcptValid); err != nil {
		t.Fatalf("failed to set TO: %s", err)
	}
	message.Subject("snapshot test")
	message.SetBodyString(TypeTextPlain, "body")
	if err := snapshot.AttachTo(message); err != nil {
		t.Fatalf("failed to attach snapshot: %s", err)
	}
	if firstMsg, secondMsg := renderMessage(t, message), renderMessage(t, message); firstMsg != secondMsg {
		t.Errorf("expected two message builds of the same snapshot to be identical")
	}
}

func TestSnapshotter_SnapshotFile_RejectsModified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(path, []byte("original content"), 0o600); err != nil {
		t.Fatalf("failed to write test file: %s", err)
	}
	snapshotter := NewSnapshotter()
	snapshot, err := snapshotter.SnapshotFile(path)
	if err != nil {
		t.Fatalf("failed to snapshot file: %s", err)
	}

	t.Run("size change is rejected", func(t *testing.T) {
		if err := os.WriteFile(path, []byte("original content with more data"), 0o600); err != nil {
			t.Fatalf("failed to modify test file: %s", err)
		}
		if _, err = snapshot.File(); !errors.Is(err, ErrSnapshotFileChanged) {
			t.Errorf("expected ErrSnapshotFileChanged, got: %v", err)
		}
	})

	t.Run("same-size content change is rejected", func(t *testing.T) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("failed to stat test file: %s", err)
		}
		// Restore the original content length and the recorded modification time, so
		// that only the SHA-256 identity check can catch the modification.
		modified := []byte("ORIGINAL CONTENT WITH MORE DATA")
		if err := os.WriteFile(path, modified, 0o600); err != nil {
			t.Fatalf("failed to modify test file: %s", err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatalf("failed to restore modification time: %s", err)
		}
		if _, err = snapshot.File(); !errors.Is(err, ErrSnapshotFileChanged) {
			t.Errorf("expected ErrSnapshotFileChanged, got: %v", err)
		}
	})

	t.Run("explicit factory rebuild accepts new content", func(t *testing.T) {
		rebuilt, err := snapshotter.SnapshotFile(path)
		if err != nil {
			t.Fatalf("failed to re-snapshot modified file: %s", err)
		}
		content := writeSnapshot(t, rebuilt)
		if string(content) != "ORIGINAL CONTENT WITH MORE DATA" {
			t.Errorf("expected rebuilt snapshot to serve new content, got %q", content)
		}
	})

	t.Run("deleted file is rejected", func(t *testing.T) {
		gonePath := filepath.Join(dir, "gone.txt")
		if err := os.WriteFile(gonePath, []byte("gone"), 0o600); err != nil {
			t.Fatalf("failed to write test file: %s", err)
		}
		goneSnapshot, err := snapshotter.SnapshotFile(gonePath)
		if err != nil {
			t.Fatalf("failed to snapshot file: %s", err)
		}
		if err := os.Remove(gonePath); err != nil {
			t.Fatalf("failed to remove test file: %s", err)
		}
		if _, err = goneSnapshot.File(); err == nil {
			t.Error("expected error for deleted snapshot source, got nil")
		}
	})
}

func TestSnapshotter_BindReader_NotReplayable(t *testing.T) {
	// io.MultiReader intentionally hides the Seeker of the strings.Reader, making it a
	// truly non-replayable reader.
	reader := io.MultiReader(strings.NewReader("one-shot-data"))
	snapshot, err := NewSnapshotter().BindReader("once.txt", reader)
	if err != nil {
		t.Fatalf("failed to bind reader: %s", err)
	}
	if snapshot.Size() != -1 {
		t.Errorf("expected size -1 for one-shot snapshot, got %d", snapshot.Size())
	}

	file, err := snapshot.File()
	if err != nil {
		t.Fatalf("failed to build file from one-shot snapshot: %s", err)
	}
	buffer := bytes.Buffer{}
	if _, err = file.Writer(&buffer); err != nil {
		t.Fatalf("failed to write one-shot content: %s", err)
	}
	if buffer.String() != "one-shot-data" {
		t.Errorf("expected one-shot content %q, got %q", "one-shot-data", buffer.String())
	}

	// The consumed reader must not be reused directly: a second write on the same File
	// (e.g. triggered by a send retry) fails explicitly.
	if _, err = file.Writer(&buffer); !errors.Is(err, ErrSnapshotConsumed) {
		t.Errorf("expected ErrSnapshotConsumed on second write, got: %v", err)
	}
	// Building a new File from the consumed snapshot fails explicitly as well.
	if _, err = snapshot.File(); !errors.Is(err, ErrSnapshotConsumed) {
		t.Errorf("expected ErrSnapshotConsumed on second build, got: %v", err)
	}
}

func TestSnapshotter_SnapshotReader_ReplayableAndBounded(t *testing.T) {
	t.Run("reader snapshot replays identically", func(t *testing.T) {
		snapshot, err := NewSnapshotter().SnapshotReader("a.bin", strings.NewReader("0123456789"))
		if err != nil {
			t.Fatalf("failed to snapshot reader: %s", err)
		}
		first := writeSnapshot(t, snapshot)
		second := writeSnapshot(t, snapshot)
		if !bytes.Equal(first, second) || string(first) != "0123456789" {
			t.Errorf("expected identical replayed content %q, got %q and %q", "0123456789", first, second)
		}
	})

	t.Run("total cache budget is enforced", func(t *testing.T) {
		snapshotter := NewSnapshotter(WithSnapshotCacheLimit(16))
		if _, err := snapshotter.SnapshotReader("a.bin", strings.NewReader("0123456789")); err != nil {
			t.Fatalf("failed to snapshot reader within budget: %s", err)
		}
		// Only 6 bytes of the total budget remain, 10 more must be refused instead of
		// being read into memory without bounds.
		if _, err := snapshotter.SnapshotReader("b.bin", strings.NewReader("0123456789")); !errors.Is(err, ErrSnapshotCacheLimit) {
			t.Errorf("expected ErrSnapshotCacheLimit, got: %v", err)
		}
	})

	t.Run("oversized reader is refused", func(t *testing.T) {
		snapshotter := NewSnapshotter(WithSnapshotCacheLimit(4))
		if _, err := snapshotter.SnapshotReader("big.bin", strings.NewReader("12345")); !errors.Is(err, ErrSnapshotCacheLimit) {
			t.Errorf("expected ErrSnapshotCacheLimit, got: %v", err)
		}
	})

	t.Run("nil reader is refused", func(t *testing.T) {
		snapshotter := NewSnapshotter()
		if _, err := snapshotter.SnapshotReader("nil.bin", nil); err == nil {
			t.Error("expected error for nil reader, got nil")
		}
		if _, err := snapshotter.BindReader("nil.bin", nil); err == nil {
			t.Error("expected error for nil reader, got nil")
		}
	})
}

func TestSnapshotter_SnapshotFile_StreamsLargeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.bin")
	content := bytes.Repeat([]byte("0123456789abcdef"), 4) // 64 bytes
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("failed to write test file: %s", err)
	}

	// A cache limit smaller than the file forces streaming from disk instead of caching.
	snapshot, err := NewSnapshotter(WithSnapshotCacheLimit(8)).SnapshotFile(path)
	if err != nil {
		t.Fatalf("failed to snapshot file: %s", err)
	}
	if snapshot.cached {
		t.Error("expected large file snapshot to not be cached")
	}
	first := writeSnapshot(t, snapshot)
	second := writeSnapshot(t, snapshot)
	if !bytes.Equal(first, content) || !bytes.Equal(second, content) {
		t.Errorf("expected streamed content to match file content")
	}

	// Modifying the file after the snapshot was taken is rejected, even though the
	// File was already built: the streaming writer re-verifies the identity.
	file, err := snapshot.File()
	if err != nil {
		t.Fatalf("failed to build file from snapshot: %s", err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatalf("failed to modify test file: %s", err)
	}
	if _, err = file.Writer(io.Discard); !errors.Is(err, ErrSnapshotFileChanged) {
		t.Errorf("expected ErrSnapshotFileChanged from streaming writer, got: %v", err)
	}
}

func TestSnapshotter_FilenameMIMEEncodedNotPath(t *testing.T) {
	t.Run("non-ASCII and path-like name is encoded, not used as path", func(t *testing.T) {
		snapshot, err := NewSnapshotter().SnapshotReader("../../机密报告.txt",
			strings.NewReader("content"))
		if err != nil {
			t.Fatalf("failed to snapshot reader: %s", err)
		}
		message := NewMsg()
		if err := message.From(TestSenderValid); err != nil {
			t.Fatalf("failed to set FROM: %s", err)
		}
		if err := message.To(TestRcptValid); err != nil {
			t.Fatalf("failed to set TO: %s", err)
		}
		message.SetBodyString(TypeTextPlain, "body")
		if err := snapshot.AttachTo(message); err != nil {
			t.Fatalf("failed to attach snapshot: %s", err)
		}
		output := renderMessage(t, message)
		if !strings.Contains(output, "=?UTF-8?") {
			t.Errorf("expected MIME encoded-word for non-ASCII filename, got:\n%s", output)
		}
		if strings.Contains(output, "../") || strings.Contains(output, "..\\") {
			t.Errorf("expected filename to not be treated as a path, got:\n%s", output)
		}
		if strings.Contains(output, "机密报告") {
			t.Errorf("expected non-ASCII filename to be MIME encoded, got:\n%s", output)
		}
	})

	t.Run("display name override does not change the source path", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "real.txt")
		if err := os.WriteFile(path, []byte("real content"), 0o600); err != nil {
			t.Fatalf("failed to write test file: %s", err)
		}
		snapshot, err := NewSnapshotter().SnapshotFile(path, WithFileName("../../evil.txt"))
		if err != nil {
			t.Fatalf("failed to snapshot file: %s", err)
		}
		if snapshot.Name() != "../../evil.txt" {
			t.Errorf("expected display name override, got %q", snapshot.Name())
		}
		content := writeSnapshot(t, snapshot)
		if string(content) != "real content" {
			t.Errorf("expected content from original path, got %q", content)
		}
		message := NewMsg()
		if err := message.From(TestSenderValid); err != nil {
			t.Fatalf("failed to set FROM: %s", err)
		}
		if err := message.To(TestRcptValid); err != nil {
			t.Fatalf("failed to set TO: %s", err)
		}
		message.SetBodyString(TypeTextPlain, "body")
		if err := snapshot.AttachTo(message); err != nil {
			t.Fatalf("failed to attach snapshot: %s", err)
		}
		output := renderMessage(t, message)
		if strings.Contains(output, `filename="../`) || strings.Contains(output, `filename="..\`) {
			t.Errorf("expected filename to not be treated as a path, got:\n%s", output)
		}
		if !strings.Contains(output, `filename=".._.._evil.txt"`) {
			t.Errorf("expected sanitized filename in Content-Disposition, got:\n%s", output)
		}
	})
}

// startSnapshotTestServer starts a simple fake SMTP server for the snapshot tests and
// returns the server properties (holding the echo buffer) and the port it listens on.
func startSnapshotTestServer(t *testing.T) (*serverProps, int) {
	t.Helper()
	ctx := t.Context()
	PortAdder.Add(1)
	serverPort := int(TestServerPortBase + PortAdder.Load())
	featureSet := "250-AUTH PLAIN\r\n250-8BITMIME\r\n250-DSN\r\n250 SMTPUTF8"
	echoBuffer := bytes.NewBuffer(nil)
	props := &serverProps{FeatureSet: featureSet, ListenPort: serverPort, EchoBuffer: echoBuffer}
	go func() {
		if err := simpleSMTPServer(ctx, t, props); err != nil {
			t.Errorf("failed to start test server: %s", err)
		}
	}()
	time.Sleep(time.Millisecond * 30)
	return props, serverPort
}

// serverEcho returns everything the fake SMTP server has received and sent so far.
func serverEcho(props *serverProps) string {
	props.BufferMutex.RLock()
	defer props.BufferMutex.RUnlock()
	buffer, ok := props.EchoBuffer.(*bytes.Buffer)
	if !ok {
		return ""
	}
	return buffer.String()
}

func newSnapshotTestMessage(t *testing.T) *Msg {
	t.Helper()
	message := NewMsg()
	if err := message.From(TestSenderValid); err != nil {
		t.Fatalf("failed to set FROM: %s", err)
	}
	if err := message.To(TestRcptValid); err != nil {
		t.Fatalf("failed to set TO: %s", err)
	}
	message.Subject("snapshot test")
	message.SetBodyString(TypeTextPlain, "snapshot body")
	return message
}

func TestClient_SendWithSnapshot_FakeSMTP(t *testing.T) {
	props, serverPort := startSnapshotTestServer(t)

	path := filepath.Join(t.TempDir(), "report.txt")
	content := []byte("fake-smtp-attachment-content")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("failed to write test file: %s", err)
	}
	snapshot, err := NewSnapshotter().SnapshotFile(path, WithFileName("附件-报告.txt"))
	if err != nil {
		t.Fatalf("failed to snapshot file: %s", err)
	}
	message := newSnapshotTestMessage(t)
	if err := snapshot.AttachTo(message); err != nil {
		t.Fatalf("failed to attach snapshot: %s", err)
	}

	client, err := NewClient(DefaultHost, WithPort(serverPort), WithTLSPolicy(NoTLS))
	if err != nil {
		t.Fatalf("failed to create client: %s", err)
	}
	if err = client.DialAndSend(message); err != nil {
		t.Fatalf("failed to send message with snapshot attachment: %s", err)
	}

	echo := serverEcho(props)
	if !strings.Contains(echo, "DATA") {
		t.Errorf("expected fake SMTP server to receive DATA, got:\n%s", echo)
	}
	if !strings.Contains(echo, base64.StdEncoding.EncodeToString(content)) {
		t.Errorf("expected attachment content in transmitted message, got:\n%s", echo)
	}
	if !strings.Contains(echo, "=?UTF-8?") {
		t.Errorf("expected MIME encoded filename in transmitted message, got:\n%s", echo)
	}
}

func TestClient_SendSnapshotChangedFile_NoPartialMessage(t *testing.T) {
	props, serverPort := startSnapshotTestServer(t)

	path := filepath.Join(t.TempDir(), "changing.txt")
	if err := os.WriteFile(path, []byte("content before send"), 0o600); err != nil {
		t.Fatalf("failed to write test file: %s", err)
	}
	snapshot, err := NewSnapshotter().SnapshotFile(path)
	if err != nil {
		t.Fatalf("failed to snapshot file: %s", err)
	}
	message := newSnapshotTestMessage(t)
	if err := snapshot.AttachTo(message); err != nil {
		t.Fatalf("failed to attach snapshot: %s", err)
	}

	// Modify the source file after the snapshot was taken and attached. The snapshot
	// identity verification must reject the build during sending.
	if err := os.WriteFile(path, []byte("content changed after snapshot"), 0o600); err != nil {
		t.Fatalf("failed to modify test file: %s", err)
	}

	client, err := NewClient(DefaultHost, WithPort(serverPort), WithTLSPolicy(NoTLS))
	if err != nil {
		t.Fatalf("failed to create client: %s", err)
	}
	err = client.DialAndSend(message)
	if err == nil {
		t.Fatal("expected send to fail for changed snapshot source, got nil")
	}
	var sendErr *SendError
	if !errors.As(err, &sendErr) || sendErr.Reason != ErrWriteContent {
		t.Errorf("expected SendError with reason ErrWriteContent, got: %v", err)
	}
	if message.IsDelivered() {
		t.Error("expected message to not be marked as delivered")
	}

	// The failed build must not hand an incomplete message to the SMTP server: the
	// server must never have seen a DATA command.
	if echo := serverEcho(props); strings.Contains(echo, "DATA") {
		t.Errorf("expected no DATA transmission on failed snapshot build, got:\n%s", echo)
	}
}

func TestClient_SendOneShotSnapshot_RetryRejected(t *testing.T) {
	props, serverPort := startSnapshotTestServer(t)

	snapshot, err := NewSnapshotter().BindReader("once.txt",
		io.MultiReader(strings.NewReader("one-shot-data")))
	if err != nil {
		t.Fatalf("failed to bind reader: %s", err)
	}
	message := newSnapshotTestMessage(t)
	if err := snapshot.AttachTo(message); err != nil {
		t.Fatalf("failed to attach snapshot: %s", err)
	}

	client, err := NewClient(DefaultHost, WithPort(serverPort), WithTLSPolicy(NoTLS))
	if err != nil {
		t.Fatalf("failed to create client: %s", err)
	}
	if err = client.DialAndSend(message); err != nil {
		t.Fatalf("failed to send message with one-shot attachment: %s", err)
	}

	// A send retry must not silently reuse the already consumed reader. It has to fail
	// explicitly instead of delivering an empty or incomplete attachment.
	err = client.DialAndSend(message)
	if err == nil {
		t.Fatal("expected send retry to fail for consumed one-shot reader, got nil")
	}
	var sendErr *SendError
	if !errors.As(err, &sendErr) || sendErr.Reason != ErrWriteContent {
		t.Errorf("expected SendError with reason ErrWriteContent, got: %v", err)
	}

	// Exactly one message (the first, complete one) must have been transmitted.
	if echo := serverEcho(props); strings.Count(echo, "DATA\r\n") != 1 {
		t.Errorf("expected exactly one DATA transmission, got:\n%s", echo)
	}
}
