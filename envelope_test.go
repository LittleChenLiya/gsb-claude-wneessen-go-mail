// SPDX-FileCopyrightText: The go-mail Authors
//
// SPDX-License-Identifier: MIT

package mail

import (
	"bytes"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestMsg_EnvelopePlan(t *testing.T) {
	t.Run("display policy and envelope recipients are computed separately", func(t *testing.T) {
		message := NewMsg()
		if err := message.From("valid-from@domain.tld"); err != nil {
			t.Fatalf("failed to set from address: %s", err)
		}
		if err := message.To("toni.tester@example.com"); err != nil {
			t.Fatalf("failed to set to address: %s", err)
		}
		if err := message.Cc("tina.tester@example.com"); err != nil {
			t.Fatalf("failed to set cc address: %s", err)
		}
		if err := message.Bcc("troy.tester@example.com"); err != nil {
			t.Fatalf("failed to set bcc address: %s", err)
		}

		plan, err := message.EnvelopePlan()
		if err != nil {
			t.Fatalf("failed to compute envelope plan: %s", err)
		}
		if plan.Sender != "<valid-from@domain.tld>" {
			t.Errorf("expected sender %q, got %q", "<valid-from@domain.tld>", plan.Sender)
		}
		if len(plan.To) != 1 || len(plan.Cc) != 1 {
			t.Errorf("expected 1 To and 1 Cc display address, got %d To and %d Cc",
				len(plan.To), len(plan.Cc))
		}
		if len(plan.Recipients) != 3 {
			t.Fatalf("expected 3 envelope recipients, got %d", len(plan.Recipients))
		}
		sources := []AddrHeader{HeaderTo, HeaderCc, HeaderBcc}
		for i, rcpt := range plan.Recipients {
			if rcpt.Source != sources[i] {
				t.Errorf("expected recipient %d source %s, got %s", i, sources[i], rcpt.Source)
			}
		}
	})
	t.Run("to and bcc with same address are sent only once", func(t *testing.T) {
		message := NewMsg()
		if err := message.From("valid-from@domain.tld"); err != nil {
			t.Fatalf("failed to set from address: %s", err)
		}
		if err := message.To("toni.tester@example.com"); err != nil {
			t.Fatalf("failed to set to address: %s", err)
		}
		if err := message.Bcc("toni.tester@example.com"); err != nil {
			t.Fatalf("failed to set bcc address: %s", err)
		}

		plan, err := message.EnvelopePlan()
		if err != nil {
			t.Fatalf("failed to compute envelope plan: %s", err)
		}
		if len(plan.Recipients) != 1 {
			t.Fatalf("expected 1 envelope recipient, got %d", len(plan.Recipients))
		}
		if plan.Recipients[0].Source != HeaderTo {
			t.Errorf("expected original source %s to be kept, got %s", HeaderTo,
				plan.Recipients[0].Source)
		}
		rcpts := plan.RcptList()
		if len(rcpts) != 1 || rcpts[0] != "<toni.tester@example.com>" {
			t.Errorf("expected single rcpt <toni.tester@example.com>, got %v", rcpts)
		}
	})
	t.Run("display name does not change mailbox identity", func(t *testing.T) {
		message := NewMsg()
		if err := message.From("valid-from@domain.tld"); err != nil {
			t.Fatalf("failed to set from address: %s", err)
		}
		if err := message.To(`"Alice Example" <alice@example.com>`); err != nil {
			t.Fatalf("failed to set to address: %s", err)
		}
		if err := message.Bcc(`"Alice Admin" <alice@example.com>`); err != nil {
			t.Fatalf("failed to set bcc address: %s", err)
		}

		plan, err := message.EnvelopePlan()
		if err != nil {
			t.Fatalf("failed to compute envelope plan: %s", err)
		}
		if len(plan.Recipients) != 1 {
			t.Fatalf("expected 1 envelope recipient despite different display names, got %d",
				len(plan.Recipients))
		}
		if plan.Recipients[0].Name != "Alice Example" {
			t.Errorf("expected display name of first occurrence to be kept, got %q",
				plan.Recipients[0].Name)
		}
	})
	t.Run("deduplication normalizes domain case", func(t *testing.T) {
		message := NewMsg()
		if err := message.From("valid-from@domain.tld"); err != nil {
			t.Fatalf("failed to set from address: %s", err)
		}
		if err := message.To("toni.tester@EXAMPLE.com"); err != nil {
			t.Fatalf("failed to set to address: %s", err)
		}
		if err := message.Cc("toni.tester@example.COM"); err != nil {
			t.Fatalf("failed to set cc address: %s", err)
		}

		plan, err := message.EnvelopePlan()
		if err != nil {
			t.Fatalf("failed to compute envelope plan: %s", err)
		}
		if len(plan.Recipients) != 1 {
			t.Fatalf("expected 1 envelope recipient, got %d", len(plan.Recipients))
		}
	})
	t.Run("invalid second address fails the whole plan and leaves the msg unchanged", func(t *testing.T) {
		message := NewMsg()
		if err := message.From("valid-from@domain.tld"); err != nil {
			t.Fatalf("failed to set from address: %s", err)
		}
		if err := message.To("toni.tester@example.com"); err != nil {
			t.Fatalf("failed to set to address: %s", err)
		}
		// Inject an invalid second address through the unchecked mail.Address setter.
		message.AddToMailAddress(&mail.Address{Address: "not-a-valid-address"})

		beforeTo := message.GetToString()
		if len(beforeTo) != 2 {
			t.Fatalf("expected 2 to addresses before planning, got %d", len(beforeTo))
		}
		plan, err := message.EnvelopePlan()
		if err == nil {
			t.Fatal("expected envelope plan to fail on invalid second address")
		}
		if plan != nil {
			t.Errorf("expected no plan to be returned, got %#v", plan)
		}
		var planErr *EnvelopePlanError
		if !errors.As(err, &planErr) {
			t.Fatalf("expected EnvelopePlanError, got %T", err)
		}
		if planErr.Header != HeaderTo {
			t.Errorf("expected failing header %s, got %s", HeaderTo, planErr.Header)
		}
		afterTo := message.GetToString()
		if len(afterTo) != 2 || afterTo[0] != beforeTo[0] || afterTo[1] != beforeTo[1] {
			t.Errorf("msg was modified by failed plan computation. Before: %v, after: %v",
				beforeTo, afterTo)
		}
		if message.IsDelivered() {
			t.Error("msg must not be marked as delivered after failed plan computation")
		}
	})
	t.Run("group syntax is expanded and not mistaken for a single address", func(t *testing.T) {
		message := NewMsg()
		if err := message.From("valid-from@domain.tld"); err != nil {
			t.Fatalf("failed to set from address: %s", err)
		}
		// Inject RFC 5322 group syntax through the unchecked mail.Address setter.
		message.AddToMailAddress(&mail.Address{
			Address: "Team: toni.tester@example.com, tina.tester@example.com;",
		})

		plan, err := message.EnvelopePlan()
		if err != nil {
			t.Fatalf("failed to compute envelope plan: %s", err)
		}
		if len(plan.Recipients) != 2 {
			t.Fatalf("expected group to expand to 2 envelope recipients, got %d",
				len(plan.Recipients))
		}
		if plan.Recipients[0].Address != "toni.tester@example.com" {
			t.Errorf("expected first group member toni.tester@example.com, got %s",
				plan.Recipients[0].Address)
		}
		if plan.Recipients[1].Address != "tina.tester@example.com" {
			t.Errorf("expected second group member tina.tester@example.com, got %s",
				plan.Recipients[1].Address)
		}
	})
	t.Run("broken group syntax fails validation", func(t *testing.T) {
		message := NewMsg()
		if err := message.From("valid-from@domain.tld"); err != nil {
			t.Fatalf("failed to set from address: %s", err)
		}
		message.AddToMailAddress(&mail.Address{Address: "Team: not-an-address;"})

		if _, err := message.EnvelopePlan(); err == nil {
			t.Fatal("expected envelope plan to fail on broken group syntax")
		}
	})
	t.Run("no from address fails with ErrNoFromAddress", func(t *testing.T) {
		message := NewMsg()
		if err := message.To("toni.tester@example.com"); err != nil {
			t.Fatalf("failed to set to address: %s", err)
		}
		_, err := message.EnvelopePlan()
		if !errors.Is(err, ErrNoFromAddress) {
			t.Errorf("expected ErrNoFromAddress, got %s", err)
		}
	})
	t.Run("no recipients fails with ErrNoRcptAddresses", func(t *testing.T) {
		message := NewMsg()
		if err := message.From("valid-from@domain.tld"); err != nil {
			t.Fatalf("failed to set from address: %s", err)
		}
		_, err := message.EnvelopePlan()
		if !errors.Is(err, ErrNoRcptAddresses) {
			t.Errorf("expected ErrNoRcptAddresses, got %s", err)
		}
	})
	t.Run("quoted local-part remains a valid single mailbox (issue #495)", func(t *testing.T) {
		message := NewMsg()
		if err := message.From("valid-from@domain.tld"); err != nil {
			t.Fatalf("failed to set from address: %s", err)
		}
		addr := `"toni.tester@example.com> ORCPT=admin@admin.com"@example.com`
		if err := message.To(addr); err != nil {
			t.Fatalf("failed to set to address: %s", err)
		}
		plan, err := message.EnvelopePlan()
		if err != nil {
			t.Fatalf("failed to compute envelope plan: %s", err)
		}
		if len(plan.Recipients) != 1 {
			t.Fatalf("expected 1 envelope recipient, got %d", len(plan.Recipients))
		}
	})
}

func TestMsg_EnvelopePlan_BccNotInPublicHeaders(t *testing.T) {
	message := NewMsg()
	if err := message.From("valid-from@domain.tld"); err != nil {
		t.Fatalf("failed to set from address: %s", err)
	}
	if err := message.To("toni.tester@example.com"); err != nil {
		t.Fatalf("failed to set to address: %s", err)
	}
	if err := message.Cc("tina.tester@example.com"); err != nil {
		t.Fatalf("failed to set cc address: %s", err)
	}
	if err := message.Bcc("troy.tester@example.com"); err != nil {
		t.Fatalf("failed to set bcc address: %s", err)
	}
	message.Subject("Bcc visibility test")
	message.SetBodyString(TypeTextPlain, "Testmail")

	// The envelope plan contains the Bcc recipient...
	plan, err := message.EnvelopePlan()
	if err != nil {
		t.Fatalf("failed to compute envelope plan: %s", err)
	}
	foundBcc := false
	for _, rcpt := range plan.Recipients {
		if rcpt.Source == HeaderBcc {
			foundBcc = true
		}
	}
	if !foundBcc {
		t.Error("expected Bcc address to be part of the envelope recipients")
	}

	// ...but the public message headers must not contain any Bcc header.
	var buf bytes.Buffer
	if _, err = message.WriteTo(&buf); err != nil {
		t.Fatalf("failed to write message to buffer: %s", err)
	}
	for _, line := range strings.Split(buf.String(), "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Errorf("public headers must not contain a Bcc header, found: %q", line)
		}
	}
	if !strings.Contains(buf.String(), "To:") || !strings.Contains(buf.String(), "Cc:") {
		t.Error("expected To and Cc headers to be present in the public headers")
	}
	if strings.Contains(buf.String(), "troy.tester@example.com") {
		t.Error("Bcc address must not appear anywhere in the public message")
	}
}

func TestClient_SendWithEnvelopePlan(t *testing.T) {
	t.Run("to and bcc with same address result in a single RCPT TO", func(t *testing.T) {
		ctx := t.Context()
		PortAdder.Add(1)
		serverPort := int(TestServerPortBase + PortAdder.Load())
		featureSet := "250-8BITMIME\r\n250-DSN\r\n250 SMTPUTF8"
		echoBuffer := bytes.NewBuffer(nil)
		props := &serverProps{
			EchoBuffer: echoBuffer,
			FeatureSet: featureSet,
			ListenPort: serverPort,
		}
		go func() {
			if err := simpleSMTPServer(ctx, t, props); err != nil {
				t.Errorf("failed to start test server: %s", err)
				return
			}
		}()
		time.Sleep(time.Millisecond * 30)

		message := testMessage(t)
		if err := message.Bcc(TestRcptValid); err != nil {
			t.Fatalf("failed to set bcc address: %s", err)
		}

		client, err := NewClient(DefaultHost, WithPort(serverPort), WithTLSPolicy(NoTLS))
		if err != nil {
			t.Fatalf("failed to create new client: %s", err)
		}
		if err = client.DialAndSendWithContext(ctx, message); err != nil {
			t.Fatalf("failed to send message: %s", err)
		}

		props.BufferMutex.RLock()
		transcript := echoBuffer.String()
		props.BufferMutex.RUnlock()
		if count := strings.Count(transcript, "RCPT TO:"); count != 1 {
			t.Errorf("expected exactly 1 RCPT TO command for duplicate To/Bcc address, got %d",
				count)
		}
		if strings.Contains(transcript, "Bcc:") {
			t.Error("transmitted message must not contain a Bcc header")
		}
	})
	t.Run("invalid second address results in zero send attempts and unmodified msg", func(t *testing.T) {
		ctx := t.Context()
		PortAdder.Add(1)
		serverPort := int(TestServerPortBase + PortAdder.Load())
		featureSet := "250-8BITMIME\r\n250-DSN\r\n250 SMTPUTF8"
		echoBuffer := bytes.NewBuffer(nil)
		props := &serverProps{
			EchoBuffer: echoBuffer,
			FeatureSet: featureSet,
			ListenPort: serverPort,
		}
		go func() {
			if err := simpleSMTPServer(ctx, t, props); err != nil {
				t.Errorf("failed to start test server: %s", err)
				return
			}
		}()
		time.Sleep(time.Millisecond * 30)

		message := testMessage(t)
		// Inject an invalid second recipient through the unchecked mail.Address setter.
		message.AddToMailAddress(&mail.Address{Address: "not-a-valid-address"})
		beforeTo := message.GetToString()

		client, err := NewClient(DefaultHost, WithPort(serverPort), WithTLSPolicy(NoTLS))
		if err != nil {
			t.Fatalf("failed to create new client: %s", err)
		}
		err = client.DialAndSendWithContext(ctx, message)
		if err == nil {
			t.Fatal("expected send to fail on invalid second address")
		}
		var sendErr *SendError
		if !errors.As(err, &sendErr) {
			t.Fatalf("expected SendError, got %T", err)
		}
		if sendErr.Reason != ErrGetRcpts {
			t.Errorf("expected ErrGetRcpts, got %s", sendErr.Reason)
		}

		props.BufferMutex.RLock()
		transcript := echoBuffer.String()
		props.BufferMutex.RUnlock()
		if strings.Contains(transcript, "MAIL FROM:") {
			t.Error("no MAIL FROM command must be issued when address validation fails")
		}
		if strings.Contains(transcript, "RCPT TO:") {
			t.Error("no RCPT TO command must be issued when address validation fails")
		}

		// The failed send must not modify the original Msg.
		afterTo := message.GetToString()
		if len(afterTo) != len(beforeTo) {
			t.Errorf("msg was modified by failed send. Before: %v, after: %v", beforeTo, afterTo)
		}
		for i := range beforeTo {
			if beforeTo[i] != afterTo[i] {
				t.Errorf("msg was modified by failed send. Before: %v, after: %v", beforeTo, afterTo)
				break
			}
		}
		if message.IsDelivered() {
			t.Error("msg must not be marked as delivered after failed send")
		}
	})
}

// ExampleMsg_EnvelopePlan demonstrates the explicit envelope and header address plan with
// four addresses: a "To" and a "Cc" address that are displayed in the public headers and
// two "Bcc" addresses that are only used as envelope recipients. The "Bcc" address that
// duplicates the "To" address (only the display name differs) is sent to only once.
func ExampleMsg_EnvelopePlan() {
	message := NewMsg()
	if err := message.From("sender@example.com"); err != nil {
		panic(err)
	}
	if err := message.To(`"Alice Example" <alice@example.com>`); err != nil {
		panic(err)
	}
	if err := message.Cc("bob@example.com"); err != nil {
		panic(err)
	}
	if err := message.Bcc(`"Alice Admin" <alice@example.com>`, "carol@example.com"); err != nil {
		panic(err)
	}

	plan, err := message.EnvelopePlan()
	if err != nil {
		panic(err)
	}
	fmt.Printf("Sender: %s\n", plan.Sender)
	fmt.Printf("Public To header: %d address(es)\n", len(plan.To))
	fmt.Printf("Public Cc header: %d address(es)\n", len(plan.Cc))
	fmt.Printf("Envelope recipients: %d\n", len(plan.Recipients))
	for _, rcpt := range plan.Recipients {
		fmt.Printf("- %s (source: %s)\n", rcpt.Address, rcpt.Source)
	}

	// Output:
	// Sender: <sender@example.com>
	// Public To header: 1 address(es)
	// Public Cc header: 1 address(es)
	// Envelope recipients: 3
	// - alice@example.com (source: To)
	// - bob@example.com (source: Cc)
	// - carol@example.com (source: Bcc)
}
