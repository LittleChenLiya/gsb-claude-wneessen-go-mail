// SPDX-FileCopyrightText: The go-mail Authors
//
// SPDX-License-Identifier: MIT

package mail

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestCIDCheck_Valid verifies that a legal cid: reference which resolves to a unique embedded
// file passes the check and the Msg is rendered as usual.
func TestCIDCheck_Valid(t *testing.T) {
	html := `<html><body><img src="cid:logo.png"></body></html>`
	msg := NewMsg(WithCIDCheck())
	msg.SetBodyString(TypeTextHTML, html)
	if err := msg.EmbedReader("logo.png", strings.NewReader("PNGDATA"),
		WithFileContentID("<logo.png>")); err != nil {
		t.Fatalf("failed to embed file: %s", err)
	}

	if err := msg.CheckCIDRefs(); err != nil {
		t.Fatalf("expected no error for valid cid reference, got: %s", err)
	}

	var buffer bytes.Buffer
	numBytes, err := msg.Write(&buffer)
	if err != nil {
		t.Fatalf("expected MIME output to succeed, got error: %s", err)
	}
	if numBytes == 0 || buffer.Len() == 0 {
		t.Fatalf("expected MIME output to be written, got %d bytes", numBytes)
	}
	output := buffer.String()
	if !strings.Contains(output, "multipart/related") {
		t.Errorf("expected multipart/related MIME output, got: %s", output)
	}
	if !strings.Contains(output, "Content-Id: <logo.png>") {
		t.Errorf("expected Content-ID header in MIME output, got: %s", output)
	}
}

// TestCIDCheck_Missing verifies that a cid: reference without a matching embedded file is
// rejected before any MIME output is generated and that the error wraps ErrCIDMissingTarget.
func TestCIDCheck_Missing(t *testing.T) {
	html := `<html><body><img src="cid:missing.png"></body></html>`
	msg := NewMsg(WithCIDCheck())
	msg.SetBodyString(TypeTextHTML, html)
	if err := msg.EmbedReader("logo.png", strings.NewReader("PNGDATA"),
		WithFileContentID("<logo.png>")); err != nil {
		t.Fatalf("failed to embed file: %s", err)
	}

	err := msg.CheckCIDRefs()
	if !errors.Is(err, ErrCIDMissingTarget) {
		t.Fatalf("expected error to wrap ErrCIDMissingTarget, got: %v", err)
	}
	if !strings.Contains(err.Error(), "missing.png") {
		t.Errorf("expected error to name the missing cid, got: %s", err)
	}

	// No MIME data must be written when the check fails during the build.
	var buffer bytes.Buffer
	numBytes, writeErr := msg.Write(&buffer)
	if !errors.Is(writeErr, ErrCIDMissingTarget) {
		t.Fatalf("expected Write to return ErrCIDMissingTarget, got: %v", writeErr)
	}
	if numBytes != 0 || buffer.Len() != 0 {
		t.Fatalf("expected zero-byte MIME output on failed cid check, got %d bytes: %s",
			numBytes, buffer.String())
	}
}

// TestCIDCheck_Duplicate verifies that two embedded files sharing the same Content-ID are
// rejected before any MIME output is generated and that the error wraps ErrCIDDuplicate.
func TestCIDCheck_Duplicate(t *testing.T) {
	html := `<html><body><img src="cid:logo.png"></body></html>`
	msg := NewMsg(WithCIDCheck())
	msg.SetBodyString(TypeTextHTML, html)
	for range 2 {
		if err := msg.EmbedReader("logo.png", strings.NewReader("PNGDATA"),
			WithFileContentID("<logo.png>")); err != nil {
			t.Fatalf("failed to embed file: %s", err)
		}
	}

	err := msg.CheckCIDRefs()
	if !errors.Is(err, ErrCIDDuplicate) {
		t.Fatalf("expected error to wrap ErrCIDDuplicate, got: %v", err)
	}
	if !strings.Contains(err.Error(), "logo.png") {
		t.Errorf("expected error to name the duplicate cid, got: %s", err)
	}

	// No MIME data must be written when the check fails during the build.
	var buffer bytes.Buffer
	numBytes, writeErr := msg.Write(&buffer)
	if !errors.Is(writeErr, ErrCIDDuplicate) {
		t.Fatalf("expected Write to return ErrCIDDuplicate, got: %v", writeErr)
	}
	if numBytes != 0 || buffer.Len() != 0 {
		t.Fatalf("expected zero-byte MIME output on failed cid check, got %d bytes: %s",
			numBytes, buffer.String())
	}
}

// TestCIDCheck_PlainURLIgnored verifies that regular (non-cid) URLs in the HTML body do not
// participate in the check, even when no embedded files exist at all.
func TestCIDCheck_PlainURLIgnored(t *testing.T) {
	html := `<html><body><img src="https://example.com/image.png">` +
		`<a href="http://example.org">link</a></body></html>`
	msg := NewMsg(WithCIDCheck())
	msg.SetBodyString(TypeTextHTML, html)

	if err := msg.CheckCIDRefs(); err != nil {
		t.Fatalf("expected plain URLs to be ignored, got error: %s", err)
	}

	var buffer bytes.Buffer
	if _, err := msg.Write(&buffer); err != nil {
		t.Fatalf("expected MIME output to succeed, got error: %s", err)
	}
	if !strings.Contains(buffer.String(), "https://example.com/image.png") {
		t.Errorf("expected plain URL to be present in MIME output, got: %s", buffer.String())
	}
}

// TestCIDCheck_DefaultNotEnforced verifies that the check is opt-in: a message created without
// WithCIDCheck that references a non-existent cid is still built normally.
func TestCIDCheck_DefaultNotEnforced(t *testing.T) {
	html := `<html><body><img src="cid:does-not-exist.png"></body></html>`
	msg := NewMsg()
	msg.SetBodyString(TypeTextHTML, html)

	var buffer bytes.Buffer
	numBytes, err := msg.Write(&buffer)
	if err != nil {
		t.Fatalf("expected default build to skip cid check, got error: %s", err)
	}
	if numBytes == 0 || buffer.Len() == 0 {
		t.Fatalf("expected MIME output for default build, got zero bytes")
	}
}

// TestCIDCheck_AngleBracketNormalization verifies that the same de-angle-bracketing rule is
// applied to both the cid: reference and the Content-ID header value.
func TestCIDCheck_AngleBracketNormalization(t *testing.T) {
	t.Run("reference without brackets, Content-ID with brackets", func(t *testing.T) {
		msg := NewMsg(WithCIDCheck())
		msg.SetBodyString(TypeTextHTML, `<img src="cid:logo.png">`)
		if err := msg.EmbedReader("logo.png", strings.NewReader("PNGDATA"),
			WithFileContentID("<logo.png>")); err != nil {
			t.Fatalf("failed to embed file: %s", err)
		}
		if err := msg.CheckCIDRefs(); err != nil {
			t.Fatalf("expected bracket normalization to match, got: %s", err)
		}
	})
	t.Run("reference with brackets, Content-ID without brackets", func(t *testing.T) {
		msg := NewMsg(WithCIDCheck())
		msg.SetBodyString(TypeTextHTML, `<img src="cid:<logo.png>">`)
		if err := msg.EmbedReader("logo.png", strings.NewReader("PNGDATA"),
			WithFileContentID("logo.png")); err != nil {
			t.Fatalf("failed to embed file: %s", err)
		}
		if err := msg.CheckCIDRefs(); err != nil {
			t.Fatalf("expected bracket normalization to match, got: %s", err)
		}
	})
	t.Run("fallback Content-ID uses sanitized file name", func(t *testing.T) {
		msg := NewMsg(WithCIDCheck())
		msg.SetBodyString(TypeTextHTML, `<img src="cid:logo.png">`)
		if err := msg.EmbedReader("logo.png", strings.NewReader("PNGDATA")); err != nil {
			t.Fatalf("failed to embed file: %s", err)
		}
		if err := msg.CheckCIDRefs(); err != nil {
			t.Fatalf("expected implicit file-name Content-ID to match, got: %s", err)
		}
	})
}

// TestNormalizeContentID checks the angle bracket normalization helper directly.
func TestNormalizeContentID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "logo.png", "logo.png"},
		{"brackets", "<logo.png>", "logo.png"},
		{"only leading bracket", "<logo.png", "logo.png"},
		{"whitespace and brackets", " <logo.png> ", "logo.png"},
		{"empty brackets", "<>", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeContentID(tt.input); got != tt.want {
				t.Fatalf("normalizeContentID(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
