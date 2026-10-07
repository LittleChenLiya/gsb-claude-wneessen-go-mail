// SPDX-FileCopyrightText: The go-mail Authors
//
// SPDX-License-Identifier: MIT

package mail

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	// ErrCIDMissingTarget is returned when an HTML body references an inline attachment via a
	// "cid:" URI but no embedded File with a matching Content-ID exists in the Msg.
	ErrCIDMissingTarget = errors.New("inline Content-ID reference has no matching embedded attachment")

	// ErrCIDDuplicate is returned when more than one embedded File carries the same Content-ID,
	// which would make a "cid:" reference from the HTML body ambiguous.
	ErrCIDDuplicate = errors.New("duplicate Content-ID in embedded attachments")
)

// cidRefRegEx matches inline attachment references in HTML content, i. e. URIs with the "cid"
// scheme such as `cid:logo@example.com` as they are used in the "src" or "href" attributes of
// HTML bodies (RFC 2392). Regular URLs (http://, https://, ...) do not use the "cid" scheme and
// therefore are not matched by this expression.
var cidRefRegEx = regexp.MustCompile(`(?i)\bcid\s*:\s*([^[:space:]"')<>]+)`)

// WithCIDCheck is a MsgOption that enables the pre-build inline attachment Content-ID reference
// check.
//
// When enabled, the Msg verifies the "cid:" references found in its HTML body parts against the
// Content-IDs of its embedded files before any MIME output is generated:
//   - Every "cid:" reference must resolve to exactly one embedded file, otherwise
//     ErrCIDMissingTarget is returned and no MIME data is written.
//   - Every Content-ID must only be assigned to a single embedded file, otherwise
//     ErrCIDDuplicate is returned and no MIME data is written.
//
// The comparison normalizes both the "cid:" reference and the Content-ID header value by stripping
// a single pair of surrounding angle brackets ("<" and ">"), so that "cid:logo" matches a
// Content-ID of "<logo>". Regular URLs in the HTML body (e. g. "https://example.com/image.png")
// are not part of the check.
//
// The check is opt-in: messages created without this MsgOption keep their original behavior and
// are built without enforcing the Content-ID references.
func WithCIDCheck() MsgOption {
	return func(m *Msg) {
		m.cidCheck = true
	}
}

// CheckCIDRefs performs the inline attachment Content-ID reference check on the Msg.
//
// This method makes the same check that is run automatically before the MIME output is generated
// when the Msg was created with the WithCIDCheck option. It can be used to validate a Msg
// explicitly, independently of writing it.
//
// The Content-IDs of the embedded files are collected first. Embedded files without an explicit
// Content-ID fall back to their sanitized file name, which is the same Content-ID the msgWriter
// would assign them. Afterwards the "cid:" references of all HTML body parts are extracted and
// matched against the collected Content-IDs. Both the references and the Content-IDs are
// normalized by removing surrounding angle brackets before they are compared.
//
// Returns:
//   - nil if all "cid:" references resolve to a unique embedded file and no Content-ID is
//     assigned more than once.
//   - An error wrapping ErrCIDMissingTarget if a "cid:" reference has no matching embedded file.
//   - An error wrapping ErrCIDDuplicate if multiple embedded files share the same Content-ID.
//   - An error if the content of an HTML body part cannot be read.
func (m *Msg) CheckCIDRefs() error {
	return m.checkCIDRefs()
}

// checkCIDRefs is the internal implementation of the inline attachment Content-ID reference
// check. See CheckCIDRefs for details.
func (m *Msg) checkCIDRefs() error {
	// Collect the normalized Content-ID of every embedded file. Files without an explicit
	// Content-ID fall back to their sanitized name, matching the msgWriter behavior.
	contentIDCount := make(map[string]int)
	for _, file := range m.embeds {
		contentID, ok := file.getHeader(HeaderContentID)
		if !ok || contentID == "" {
			contentID = sanitizeFilename(file.Name)
		}
		contentIDCount[normalizeContentID(contentID)]++
	}

	duplicates := make([]string, 0)
	for contentID, count := range contentIDCount {
		if count > 1 {
			duplicates = append(duplicates, contentID)
		}
	}

	// Extract the "cid:" references from all HTML body parts. Regular URLs do not use the
	// "cid" scheme and therefore are ignored by the regular expression.
	referenced := make(map[string]struct{})
	for _, part := range m.parts {
		if part.isDeleted || part.contentType != TypeTextHTML {
			continue
		}
		content, err := part.GetContent()
		if err != nil {
			return fmt.Errorf("failed to read HTML body for Content-ID reference check: %w", err)
		}
		matches := cidRefRegEx.FindAllStringSubmatch(string(content), -1)
		for _, match := range matches {
			if len(match) < 2 {
				continue
			}
			contentID := normalizeContentID(match[1])
			if contentID == "" {
				continue
			}
			referenced[contentID] = struct{}{}
		}
	}

	missing := make([]string, 0)
	for contentID := range referenced {
		if _, ok := contentIDCount[contentID]; !ok {
			missing = append(missing, contentID)
		}
	}

	var errs []error
	if len(missing) > 0 {
		sort.Strings(missing)
		errs = append(errs, fmt.Errorf("%w: %s", ErrCIDMissingTarget, strings.Join(missing, ", ")))
	}
	if len(duplicates) > 0 {
		sort.Strings(duplicates)
		errs = append(errs, fmt.Errorf("%w: %s", ErrCIDDuplicate, strings.Join(duplicates, ", ")))
	}
	return errors.Join(errs...)
}

// normalizeContentID normalizes a Content-ID or a "cid:" reference for comparison by stripping a
// single pair of surrounding angle brackets ("<" and ">"). The same rule is applied to both
// sides, so that a "cid:logo" reference matches a Content-ID of "<logo>". Whitespace is trimmed
// as well.
func normalizeContentID(contentID string) string {
	contentID = strings.TrimSpace(contentID)
	contentID = strings.TrimPrefix(contentID, "<")
	contentID = strings.TrimSuffix(contentID, ">")
	return strings.TrimSpace(contentID)
}
