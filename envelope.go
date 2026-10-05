// SPDX-FileCopyrightText: The go-mail Authors
//
// SPDX-License-Identifier: MIT

package mail

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// EnvelopeRecipient represents a single envelope (SMTP "RCPT TO") recipient of a Msg.
//
// The EnvelopeRecipient carries the normalized mailbox address that is used for the SMTP
// transmission as well as the display name and the address header the recipient originates
// from. The display name is informational only - it never influences the identity of the
// mailbox. Two EnvelopeRecipient entries with the same normalized address are considered
// identical, regardless of their display names.
type EnvelopeRecipient struct {
	// Address is the normalized addr-spec (mailbox) of the recipient, without any display
	// name or angle brackets. This is the value that is used for the SMTP "RCPT TO" command.
	Address string

	// Name is the display name of the first occurrence of the recipient. It does not
	// influence the mailbox identity.
	Name string

	// Source is the address header the recipient was first found in (HeaderTo, HeaderCc or
	// HeaderBcc). The original source is preserved even if the same address appears in
	// multiple address headers.
	Source AddrHeader
}

// String returns the angle-addr representation of the EnvelopeRecipient as used for the
// SMTP "RCPT TO" command. The addr-spec is properly quoted if required (e.g. for quoted
// local-parts).
func (r EnvelopeRecipient) String() string {
	return (&mail.Address{Address: r.Address}).String()
}

// EnvelopePlan is the explicit delivery plan for a Msg. It separates the addresses that are
// displayed in the public message headers from the actual envelope recipients that are used
// for the SMTP transmission.
//
// The display policy (To and Cc) and the real recipient list are computed independently:
//   - To and Cc hold the addresses that are visible in the public message headers.
//   - Bcc addresses never appear in the public headers. They are only represented in the
//     Recipients list.
//   - Recipients is the de-duplicated union of To, Cc and Bcc. Duplicate normalized
//     addresses are only listed once, keeping the source of their first occurrence.
//
// An EnvelopePlan is only returned if every address of the Msg is valid. Computing the plan
// never modifies the Msg it was derived from.
type EnvelopePlan struct {
	// Sender is the envelope sender address in angle-addr form (e.g. "<user@example.com>")
	// as used for the SMTP "MAIL FROM" command.
	Sender string

	// To holds the addresses that are displayed in the public "To" header.
	To []*mail.Address

	// Cc holds the addresses that are displayed in the public "Cc" header.
	Cc []*mail.Address

	// Recipients is the de-duplicated list of all envelope recipients (To, Cc and Bcc).
	Recipients []EnvelopeRecipient
}

// RcptList returns the envelope recipient list of the EnvelopePlan as a slice of
// angle-addr formatted strings, suitable for the SMTP "RCPT TO" command.
func (p *EnvelopePlan) RcptList() []string {
	list := make([]string, 0, len(p.Recipients))
	for _, rcpt := range p.Recipients {
		list = append(list, rcpt.String())
	}
	return list
}

// EnvelopePlanError describes an address validation failure that was encountered while
// computing an EnvelopePlan for a Msg.
type EnvelopePlanError struct {
	// Header is the address header in which the invalid address was found. For envelope
	// sender failures this is HeaderEnvelopeFrom or HeaderFrom.
	Header AddrHeader

	// Address is the offending address value.
	Address string

	// Err is the underlying validation error.
	Err error
}

// Error returns a string representation of the EnvelopePlanError.
func (e *EnvelopePlanError) Error() string {
	if e.Address == "" {
		return fmt.Sprintf("invalid address in %s header: %s", e.Header, e.Err)
	}
	return fmt.Sprintf("invalid address %q in %s header: %s", e.Address, e.Header, e.Err)
}

// Unwrap returns the underlying error of the EnvelopePlanError.
func (e *EnvelopePlanError) Unwrap() error {
	return e.Err
}

// EnvelopePlan computes the explicit envelope and header address plan for the Msg.
//
// The display policy (the public "To" and "Cc" headers) and the real envelope recipients
// are computed separately. "Bcc" addresses are included in the envelope recipient list but
// never appear in the public headers. Recipients are de-duplicated by their normalized
// mailbox address (the display name does not influence the mailbox identity), preserving
// the source header of the first occurrence. RFC 5322 group syntax is expanded into its
// member mailboxes and never mistaken for a single address.
//
// The plan is only returned if every address of the Msg (sender and all recipients) is
// valid. Computing the plan does not modify the Msg. If any address fails validation, an
// error of type *EnvelopePlanError is returned and the Msg remains unchanged.
//
// Returns:
//   - A pointer to the EnvelopePlan and an error if any address is invalid.
func (m *Msg) EnvelopePlan() (*EnvelopePlan, error) {
	plan := &EnvelopePlan{}

	// Compute and validate the envelope sender. The envelope sender takes precedence over
	// the "From" header, mirroring the behavior of GetSender.
	senderHeader := HeaderEnvelopeFrom
	sender, ok := m.addrHeader[HeaderEnvelopeFrom]
	if !ok || len(sender) == 0 {
		senderHeader = HeaderFrom
		sender, ok = m.addrHeader[HeaderFrom]
	}
	if !ok || len(sender) == 0 || sender[0] == nil {
		return nil, &EnvelopePlanError{Header: HeaderFrom, Err: ErrNoFromAddress}
	}
	senderAddr, err := planMailbox(sender[0], senderHeader)
	if err != nil {
		return nil, err
	}
	plan.Sender = (&mail.Address{Address: senderAddr.Address}).String()

	// Compute the display headers and the envelope recipients separately. Bcc addresses
	// are only added to the envelope recipients, never to the public headers.
	seen := make(map[string]struct{})
	for _, header := range []AddrHeader{HeaderTo, HeaderCc, HeaderBcc} {
		for _, entry := range m.addrHeader[header] {
			addresses, err := planAddresses(entry, header)
			if err != nil {
				return nil, err
			}
			for _, address := range addresses {
				switch header {
				case HeaderTo:
					plan.To = append(plan.To, address)
				case HeaderCc:
					plan.Cc = append(plan.Cc, address)
				}
				key := normalizeMailAddress(address.Address)
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				plan.Recipients = append(plan.Recipients, EnvelopeRecipient{
					Address: address.Address,
					Name:    address.Name,
					Source:  header,
				})
			}
		}
	}
	if len(plan.Recipients) == 0 {
		return nil, &EnvelopePlanError{Header: HeaderTo, Err: ErrNoRcptAddresses}
	}
	return plan, nil
}

// planMailbox validates that the given mail.Address represents exactly one valid mailbox
// and returns it. RFC 5322 group syntax in the address is expanded into its member
// mailboxes by planAddresses instead of being mistaken for a single address.
func planMailbox(addr *mail.Address, header AddrHeader) (*mail.Address, error) {
	addresses, err := planAddresses(addr, header)
	if err != nil {
		return nil, err
	}
	if len(addresses) != 1 {
		return nil, &EnvelopePlanError{
			Header: header, Address: addr.Address,
			Err: errors.New("expected exactly one mailbox"),
		}
	}
	return addresses[0], nil
}

// planAddresses validates the given mail.Address and returns the list of mailboxes it
// represents. A valid single mailbox is returned as-is. If the address contains RFC 5322
// group syntax (e.g. "Team: user@example.com, other@example.com;"), the group is expanded
// into its member mailboxes instead of being treated as a single address. Any other value
// that does not parse as a single mailbox results in an *EnvelopePlanError.
func planAddresses(addr *mail.Address, header AddrHeader) ([]*mail.Address, error) {
	if addr == nil {
		return nil, &EnvelopePlanError{
			Header: header, Err: errors.New("address is nil"),
		}
	}
	spec := strings.TrimSpace(addr.Address)
	if spec == "" {
		return nil, &EnvelopePlanError{
			Header: header, Err: errors.New("address is empty"),
		}
	}
	// Validate the formatted angle-addr form of the address. The formatted form correctly
	// re-quotes local-parts that require quoting (e.g. quoted local-parts as in issue
	// #495), which the raw addr-spec alone does not.
	formatted := (&mail.Address{Address: spec}).String()
	if parsed, err := mail.ParseAddress(formatted); err == nil {
		// A single valid mailbox. The display name of the original entry is kept, the
		// identity of the mailbox is solely defined by its addr-spec.
		return []*mail.Address{{Name: addr.Name, Address: parsed.Address}}, nil
	}
	if group, ok, err := expandGroupSyntax(spec); ok {
		if err != nil {
			return nil, &EnvelopePlanError{Header: header, Address: spec, Err: err}
		}
		return group, nil
	}
	_, parseErr := mail.ParseAddress(formatted)
	return nil, &EnvelopePlanError{Header: header, Address: spec, Err: parseErr}
}

// expandGroupSyntax expands RFC 5322 group syntax ("display-name: mailbox-list;") into its
// member mailboxes. The second return value reports whether the given value was recognized
// as group syntax. Group syntax is never mistaken for a single address.
func expandGroupSyntax(spec string) ([]*mail.Address, bool, error) {
	colon := strings.Index(spec, ":")
	semicolon := strings.LastIndex(spec, ";")
	if colon <= 0 || semicolon <= colon {
		return nil, false, nil
	}
	list := strings.TrimSpace(spec[colon+1 : semicolon])
	if list == "" {
		// An empty group expands to zero mailboxes.
		return nil, true, nil
	}
	addresses, err := mail.ParseAddressList(list)
	if err != nil {
		return nil, true, fmt.Errorf("failed to expand address group %q: %w", spec, err)
	}
	return addresses, true, nil
}

// normalizeMailAddress returns the normalized form of an addr-spec used for recipient
// de-duplication. The domain part is lower-cased while the local part is kept as-is (it is
// case-sensitive per RFC 5321). Display names are not part of the normalization and
// therefore never change the identity of a mailbox.
func normalizeMailAddress(spec string) string {
	if idx := strings.LastIndex(spec, "@"); idx >= 0 {
		return spec[:idx] + "@" + strings.ToLower(spec[idx+1:])
	}
	return strings.ToLower(spec)
}
