// SPDX-FileCopyrightText: The go-mail Authors
//
// SPDX-License-Identifier: MIT

package mail

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cidTestHTML is a short HTML body that references an inline image via a cid URI and a regular
// remote image via an https URL. The https URL must never participate in the cid validation.
const cidTestHTML = `<html><body>` +
	`<img src="cid:logo@example.com" alt="logo">` +
	`<img src="https://example.com/images/banner.png" alt="banner">` +
	`</body></html>`

// newCIDTestMsg creates a Msg with the given MsgOptions, the short cidTestHTML body and an embedded
// file carrying the given Content-ID.
func newCIDTestMsg(t *testing.T, opts []MsgOption, cid string) *Msg {
	t.Helper()
	message := NewMsg(opts...)
	_ = message.From(TestSenderValid)
	_ = message.To(TestRcptValid)
	message.Subject("CID test")
	message.SetBodyString(TypeTextHTML, cidTestHTML)
	if err := message.EmbedReader("logo.png", bytes.NewBufferString("PNGDATA"),
		WithFileContentID(cid)); err != nil {
		t.Fatalf("failed to embed reader: %s", err)
	}
	return message
}

func TestMsg_CheckInlineCIDReferences(t *testing.T) {
	t.Run("valid cid reference with angle-bracketed Content-ID passes", func(t *testing.T) {
		message := newCIDTestMsg(t, nil, "<logo@example.com>")
		if err := message.CheckInlineCIDReferences(); err != nil {
			t.Errorf("expected inline cid check to pass, but got error: %s", err)
		}
	})

	t.Run("valid cid reference with bare Content-ID passes", func(t *testing.T) {
		message := newCIDTestMsg(t, nil, "logo@example.com")
		if err := message.CheckInlineCIDReferences(); err != nil {
			t.Errorf("expected inline cid check to pass, but got error: %s", err)
		}
	})

	t.Run("missing target returns ErrInlineCIDMissingTarget", func(t *testing.T) {
		message := newCIDTestMsg(t, nil, "<other@example.com>")
		err := message.CheckInlineCIDReferences()
		if !errors.Is(err, ErrInlineCIDMissingTarget) {
			t.Fatalf("expected error %v, got: %v", ErrInlineCIDMissingTarget, err)
		}
		if !strings.Contains(err.Error(), "cid:logo@example.com") {
			t.Errorf("expected error to name the missing cid, got: %s", err)
		}
	})

	t.Run("duplicate Content-ID returns ErrInlineCIDDuplicateTarget", func(t *testing.T) {
		message := newCIDTestMsg(t, nil, "<logo@example.com>")
		if err := message.EmbedReader("logo-copy.png", bytes.NewBufferString("PNGDATA2"),
			WithFileContentID("logo@example.com")); err != nil {
			t.Fatalf("failed to embed second reader: %s", err)
		}
		err := message.CheckInlineCIDReferences()
		if !errors.Is(err, ErrInlineCIDDuplicateTarget) {
			t.Fatalf("expected error %v, got: %v", ErrInlineCIDDuplicateTarget, err)
		}
	})

	t.Run("regular http/https URLs are ignored", func(t *testing.T) {
		message := NewMsg()
		message.SetBodyString(TypeTextHTML, `<a href="https://example.com/page">link</a>`+
			`<img src="HTTP://example.com/img.png">`)
		// No embeds at all, but there are also no cid: references - the check must pass.
		if err := message.CheckInlineCIDReferences(); err != nil {
			t.Errorf("expected no error for plain URLs, got: %s", err)
		}
	})

	t.Run("plain text parts do not participate in the check", func(t *testing.T) {
		message := NewMsg()
		message.SetBodyString(TypeTextPlain, "see cid:logo@example.com in the HTML version")
		if err := message.CheckInlineCIDReferences(); err != nil {
			t.Errorf("expected plain text cid text to be ignored, got: %s", err)
		}
	})

	t.Run("default Content-ID based on file name is honored", func(t *testing.T) {
		// Without an explicit WithFileContentID the msgWriter would assign "<logo.png>" as the
		// Content-ID, so a matching "cid:logo.png" reference must validate even before rendering.
		message := NewMsg()
		message.SetBodyString(TypeTextHTML, `<img src="cid:logo.png">`)
		if err := message.EmbedReader("logo.png", bytes.NewBufferString("PNGDATA")); err != nil {
			t.Fatalf("failed to embed reader: %s", err)
		}
		if err := message.CheckInlineCIDReferences(); err != nil {
			t.Errorf("expected default Content-ID based check to pass, got: %s", err)
		}
	})
}

func TestMsg_InlineCIDCheck_WriteTo(t *testing.T) {
	t.Run("valid message is written when check is enabled", func(t *testing.T) {
		message := newCIDTestMsg(t, []MsgOption{WithInlineCIDCheck()}, "<logo@example.com>")
		buffer := bytes.NewBuffer(nil)
		written, err := message.WriteTo(buffer)
		if err != nil {
			t.Fatalf("expected WriteTo to succeed, got error: %s", err)
		}
		if written == 0 || buffer.Len() == 0 {
			t.Fatal("expected MIME output to be written, but buffer is empty")
		}
		output := buffer.String()
		if !strings.Contains(output, "multipart/related") {
			t.Errorf("expected multipart/related MIME output, got: %s", output)
		}
		// The HTML body is quoted-printable encoded (so "=" becomes "=3D") and MIME headers are
		// emitted in their canonical textproto form ("Content-Id"). Check for the stable parts.
		if !strings.Contains(output, "cid:logo@example.com") {
			t.Errorf("expected cid reference in MIME output, got: %s", output)
		}
		if !strings.Contains(output, "Content-Id: <logo@example.com>") {
			t.Errorf("expected embedded file Content-Id in MIME output, got: %s", output)
		}
	})

	t.Run("missing target is rejected before any MIME output", func(t *testing.T) {
		message := newCIDTestMsg(t, []MsgOption{WithInlineCIDCheck()}, "<other@example.com>")
		buffer := bytes.NewBuffer(nil)
		written, err := message.WriteTo(buffer)
		if !errors.Is(err, ErrInlineCIDMissingTarget) {
			t.Fatalf("expected error %v, got: %v", ErrInlineCIDMissingTarget, err)
		}
		if written != 0 {
			t.Errorf("expected zero bytes written on failure, got: %d", written)
		}
		if buffer.Len() != 0 {
			t.Errorf("expected zero MIME output on failure, got: %q", buffer.String())
		}
	})

	t.Run("duplicate target is rejected before any MIME output", func(t *testing.T) {
		message := newCIDTestMsg(t, []MsgOption{WithInlineCIDCheck()}, "<logo@example.com>")
		if err := message.EmbedReader("logo-copy.png", bytes.NewBufferString("PNGDATA2"),
			WithFileContentID("logo@example.com")); err != nil {
			t.Fatalf("failed to embed second reader: %s", err)
		}
		buffer := bytes.NewBuffer(nil)
		written, err := message.WriteTo(buffer)
		if !errors.Is(err, ErrInlineCIDDuplicateTarget) {
			t.Fatalf("expected error %v, got: %v", ErrInlineCIDDuplicateTarget, err)
		}
		if written != 0 || buffer.Len() != 0 {
			t.Errorf("expected zero bytes and zero MIME output, got written=%d buffer=%q",
				written, buffer.String())
		}
	})

	t.Run("check is not enforced on a default build", func(t *testing.T) {
		// Msg created without WithInlineCIDCheck: a dangling cid reference must not prevent the
		// MIME output.
		message := newCIDTestMsg(t, nil, "<other@example.com>")
		buffer := bytes.NewBuffer(nil)
		if _, err := message.WriteTo(buffer); err != nil {
			t.Fatalf("expected default build to skip cid validation, got error: %s", err)
		}
		if buffer.Len() == 0 {
			t.Fatal("expected MIME output for default build despite dangling cid reference")
		}
	})

	t.Run("missing target is rejected when writing to a temp file", func(t *testing.T) {
		tempDir := t.TempDir()
		message := newCIDTestMsg(t, []MsgOption{WithInlineCIDCheck()}, "<other@example.com>")
		targetFile := filepath.Join(tempDir, "inline-cid-missing.eml")
		err := message.WriteToFile(targetFile)
		if !errors.Is(err, ErrInlineCIDMissingTarget) {
			t.Fatalf("expected error %v, got: %v", ErrInlineCIDMissingTarget, err)
		}
		// WriteToFile creates the file shell before writing, but since the check rejects the
		// message before any MIME data is generated, the file must stay completely empty.
		info, statErr := os.Stat(targetFile)
		if statErr != nil {
			t.Fatalf("expected output file shell to exist, stat error: %v", statErr)
		}
		if info.Size() != 0 {
			t.Errorf("expected zero MIME output in file, got size: %d", info.Size())
		}
	})
}
