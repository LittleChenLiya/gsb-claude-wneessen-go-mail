// SPDX-FileCopyrightText: The go-mail Authors
//
// SPDX-License-Identifier: MIT

package mail

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	// ErrInlineCIDMissingTarget is returned when an HTML part references an inline attachment via
	// a "cid:" URI (Content-ID), but no embedded file with a matching Content-ID is present.
	ErrInlineCIDMissingTarget = errors.New("inline cid reference has no matching embedded attachment")

	// ErrInlineCIDDuplicateTarget is returned when more than one embedded file carries the same
	// Content-ID that is referenced via a "cid:" URI in an HTML part. The reference would be
	// ambiguous for the MUA rendering the message.
	ErrInlineCIDDuplicateTarget = errors.New("inline cid reference matches multiple embedded attachments")
)

// cidReferenceRegExp matches "cid:" URIs as they are used to reference inline content in HTML, e. g.
// <img src="cid:logo@example.com">. Only the actual cid-scheme is matched, so regular URLs such as
// "http://" or "https://" links are ignored. The captured group holds the referenced Content-ID up
// until the first quoting, whitespace or angle bracket delimiter.
var cidReferenceRegExp = regexp.MustCompile(`(?i)cid:([^"'\s<>]+)`)

// normalizeContentID applies the same de-angeling rule to a Content-ID that is used for the "cid:"
// reference found in the HTML body. RFC 2392 defines that the Content-ID is enclosed in angle
// brackets ("<id@domain>"), while the cid-URI references the bare identifier ("cid:id@domain").
//
// Both a leading "<" and a trailing ">" are stripped, if present. Values that are given without the
// angle brackets are returned unchanged.
func normalizeContentID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return id
}

// CheckInlineCIDReferences verifies that every "cid:" reference found in one of the HTML parts of the
// Msg points to exactly one embedded file (inline attachment).
//
// The Content-ID of the embedded files is compared using the same de-angeling rule as the reference
// found in the HTML body (see normalizeContentID), so both "cid:logo" in the HTML and a Content-ID
// header value of "<logo>" and "logo" are considered equal.
//
// Two conditions are rejected:
//   - A referenced Content-ID has no matching embedded file (ErrInlineCIDMissingTarget).
//   - More than one embedded file carries the same referenced Content-ID
//     (ErrInlineCIDDuplicateTarget).
//
// Only "cid:" URIs are evaluated. Regular URLs such as "http://" or "https://" links do not
// participate in the check. Plain text parts are not evaluated. Embedded files that are never
// referenced do not cause an error.
func (m *Msg) CheckInlineCIDReferences() error {
	referencedIDs := make(map[string]struct{})
	for _, part := range m.parts {
		if part == nil || part.isDeleted || part.smime {
			continue
		}
		if part.contentType != TypeTextHTML {
			continue
		}
		content, err := part.GetContent()
		if err != nil {
			return fmt.Errorf("failed to read HTML part for inline cid check: %w", err)
		}
		matches := cidReferenceRegExp.FindAllStringSubmatch(string(content), -1)
		for _, match := range matches {
			if len(match) < 2 {
				continue
			}
			referencedIDs[normalizeContentID(match[1])] = struct{}{}
		}
	}

	if len(referencedIDs) == 0 {
		return nil
	}

	contentIDCounts := make(map[string]int)
	for _, embed := range m.embeds {
		if embed == nil {
			continue
		}
		contentID, ok := embed.getHeader(HeaderContentID)
		if !ok {
			// The msgWriter assigns a default Content-ID based on the sanitized file name when
			// no explicit Content-ID has been set. Use the same value here.
			contentID = fmt.Sprintf("<%s>", sanitizeFilename(embed.Name))
		}
		contentIDCounts[normalizeContentID(contentID)]++
	}

	for referencedID := range referencedIDs {
		count, ok := contentIDCounts[referencedID]
		if !ok || count == 0 {
			return fmt.Errorf("%w: cid:%s", ErrInlineCIDMissingTarget, referencedID)
		}
		if count > 1 {
			return fmt.Errorf("%w: cid:%s", ErrInlineCIDDuplicateTarget, referencedID)
		}
	}
	return nil
}
