package agentconnectedapps

import (
	"context"
	"net/mail"
	"strings"

	"github.com/simplexai-labs/linger/apps/teamhub/internal/externalsource"
)

var (
	outlookUnexpectedRecipientKeys = []string{
		"cc", "bcc", "to_recipients", "cc_recipients", "bcc_recipients",
		"recipient_email", "extra_recipients", "recipients",
	}
	gmailUnexpectedRecipientKeys = []string{
		"to", "cc_emails", "bcc_emails", "to_recipients", "cc_recipients",
		"bcc_recipients", "recipients",
	}
)

// SetAutoApproveEmailRecipients configures bare email addresses whose outgoing
// Outlook and Gmail sends skip sponsor confirmation when every recipient (To,
// Cc, and Bcc) is in the allowlist.
func (s *Service) SetAutoApproveEmailRecipients(recipients []string) {
	if s == nil {
		return
	}
	if len(recipients) == 0 {
		s.autoApproveEmailRecipients = nil
		return
	}
	allowlist := make(map[string]struct{}, len(recipients))
	for _, recipient := range recipients {
		if normalized, ok := normalizeBareEmail(strings.TrimSpace(recipient)); ok {
			allowlist[normalized] = struct{}{}
		}
	}
	if len(allowlist) == 0 {
		s.autoApproveEmailRecipients = nil
		return
	}
	s.autoApproveEmailRecipients = allowlist
}

// emailSendAutoApproved reports whether an outgoing email tool call can run
// without sponsor confirmation because every resolved recipient is in the
// configured allowlist. Any missing, unexpected, or unparseable recipient fails
// closed so confirmation stays required. For draft sends it also returns the
// fetched draft object so prepareConfirmation can reuse it without a second
// provider read.
func (s *Service) emailSendAutoApproved(ctx context.Context, connection externalsource.AgentToolConnection, action confirmationAction) (bool, map[string]any, error) {
	if s == nil || len(s.autoApproveEmailRecipients) == 0 {
		return false, nil, nil
	}
	switch action.ToolSlug {
	case "OUTLOOK_SEND_EMAIL":
		recipients, ok := outlookSendRecipients(action.Arguments)
		return ok && s.allRecipientsAllowlisted(recipients), nil, nil
	case "GMAIL_SEND_EMAIL":
		recipients, ok := gmailSendRecipients(action.Arguments, false)
		return ok && s.allRecipientsAllowlisted(recipients), nil, nil
	case "GMAIL_REPLY_TO_THREAD":
		recipients, ok := gmailSendRecipients(action.Arguments, true)
		return ok && s.allRecipientsAllowlisted(recipients), nil, nil
	case "OUTLOOK_SEND_DRAFT":
		if !defaultMailboxUser(action.Arguments) {
			return false, nil, nil
		}
		id, ok := nonEmptyStringArg(action.Arguments, "message_id")
		if !ok {
			return false, nil, nil
		}
		data, err := s.readTool(ctx, connection, "OUTLOOK_GET_MESSAGE", action.ToolVersion, map[string]any{"message_id": id})
		if err != nil {
			return false, nil, err
		}
		recipients, ok := outlookDraftRecipients(data)
		return ok && s.allRecipientsAllowlisted(recipients), data, nil
	case "GMAIL_SEND_DRAFT":
		if !defaultMailboxUser(action.Arguments) {
			return false, nil, nil
		}
		id, ok := nonEmptyStringArg(action.Arguments, "draft_id")
		if !ok {
			return false, nil, nil
		}
		data, err := s.readTool(ctx, connection, "GMAIL_GET_DRAFT", action.ToolVersion, map[string]any{"draft_id": id, "format": "full"})
		if err != nil {
			return false, nil, err
		}
		recipients, ok := gmailDraftRecipients(data)
		return ok && s.allRecipientsAllowlisted(recipients), data, nil
	default:
		return false, nil, nil
	}
}

func (s *Service) allRecipientsAllowlisted(recipients []string) bool {
	if len(recipients) == 0 || len(s.autoApproveEmailRecipients) == 0 {
		return false
	}
	for _, recipient := range recipients {
		if _, ok := s.autoApproveEmailRecipients[recipient]; !ok {
			return false
		}
	}
	return true
}

func outlookSendRecipients(args map[string]any) ([]string, bool) {
	if hasUnexpectedRecipientKey(args, outlookUnexpectedRecipientKeys) {
		return nil, false
	}
	to, ok := extractRecipientField(args, "to", true)
	if !ok {
		return nil, false
	}
	cc, ok := extractRecipientField(args, "cc_emails", false)
	if !ok {
		return nil, false
	}
	bcc, ok := extractRecipientField(args, "bcc_emails", false)
	if !ok {
		return nil, false
	}
	return combineRecipients(to, cc, bcc), true
}

func gmailSendRecipients(args map[string]any, requirePrimary bool) ([]string, bool) {
	if hasUnexpectedRecipientKey(args, gmailUnexpectedRecipientKeys) {
		return nil, false
	}
	primary, ok := extractRecipientField(args, "recipient_email", requirePrimary)
	if !ok {
		return nil, false
	}
	extra, ok := extractRecipientField(args, "extra_recipients", false)
	if !ok {
		return nil, false
	}
	cc, ok := extractRecipientField(args, "cc", false)
	if !ok {
		return nil, false
	}
	bcc, ok := extractRecipientField(args, "bcc", false)
	if !ok {
		return nil, false
	}
	all := combineRecipients(primary, extra, cc, bcc)
	if len(all) == 0 {
		return nil, false
	}
	return all, true
}

func outlookDraftRecipients(data map[string]any) ([]string, bool) {
	var all []string
	for _, key := range []string{"toRecipients", "ccRecipients", "bccRecipients"} {
		raw, exists := data[key]
		if !exists || raw == nil {
			continue
		}
		items, ok := raw.([]any)
		if !ok {
			return nil, false
		}
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				return nil, false
			}
			addressObj, ok := entry["emailAddress"].(map[string]any)
			if !ok {
				return nil, false
			}
			rawAddress, ok := addressObj["address"].(string)
			if !ok {
				return nil, false
			}
			parsed, ok := parseEmailAddresses(rawAddress)
			if !ok {
				return nil, false
			}
			all = append(all, parsed...)
		}
	}
	if len(all) == 0 {
		return nil, false
	}
	return all, true
}

func gmailDraftRecipients(data map[string]any) ([]string, bool) {
	message, ok := data["message"].(map[string]any)
	if !ok {
		return nil, false
	}
	payload, ok := message["payload"].(map[string]any)
	if !ok {
		return nil, false
	}
	headers, ok := payload["headers"].([]any)
	if !ok {
		return nil, false
	}
	var all []string
	for _, item := range headers {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		name := strings.ToLower(strings.TrimSpace(str(entry, "name")))
		value := strings.TrimSpace(str(entry, "value"))
		switch name {
		case "to", "cc", "bcc":
			if value == "" {
				continue
			}
			parsed, ok := parseEmailAddresses(value)
			if !ok {
				return nil, false
			}
			all = append(all, parsed...)
		case "resent-to", "resent-cc", "resent-bcc":
			if value != "" {
				return nil, false
			}
		}
	}
	if len(all) == 0 {
		return nil, false
	}
	return all, true
}

// defaultMailboxUser ensures draft inspection reads the same mailbox ("me")
// that the send call targets.
func defaultMailboxUser(args map[string]any) bool {
	value, exists := args["user_id"]
	if !exists || value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && (text == "" || text == "me")
}

func nonEmptyStringArg(args map[string]any, key string) (string, bool) {
	value, ok := args[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

func hasUnexpectedRecipientKey(args map[string]any, keys []string) bool {
	for _, key := range keys {
		if value, exists := args[key]; exists && value != nil {
			return true
		}
	}
	return false
}

func extractRecipientField(args map[string]any, key string, required bool) ([]string, bool) {
	value, exists := args[key]
	if !exists || value == nil {
		return nil, !required
	}
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, !required
		}
		return parseEmailAddresses(typed)
	case []string:
		if len(typed) == 0 {
			return nil, !required
		}
		var result []string
		for _, item := range typed {
			parsed, ok := parseEmailAddresses(item)
			if !ok {
				return nil, false
			}
			result = append(result, parsed...)
		}
		return result, true
	case []any:
		if len(typed) == 0 {
			return nil, !required
		}
		var result []string
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			parsed, ok := parseEmailAddresses(text)
			if !ok {
				return nil, false
			}
			result = append(result, parsed...)
		}
		return result, true
	default:
		return nil, false
	}
}

// parseEmailAddresses extracts lowercased bare email addresses from one RFC
// 5322 address or comma-separated list. Semicolons and control characters fail
// closed because some mail providers split recipients on semicolons.
func parseEmailAddresses(raw string) ([]string, bool) {
	if strings.ContainsAny(raw, "\r\n\x00;") {
		return nil, false
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, false
	}
	parsed, err := mail.ParseAddressList(trimmed)
	if err != nil || len(parsed) == 0 {
		return nil, false
	}
	addresses := make([]string, 0, len(parsed))
	for _, entry := range parsed {
		normalized, ok := normalizeBareEmail(entry.Address)
		if !ok {
			return nil, false
		}
		addresses = append(addresses, normalized)
	}
	return addresses, true
}

func normalizeBareEmail(raw string) (string, bool) {
	if raw == "" || strings.ContainsAny(raw, " \t\r\n\x00<>\"(),;:\\[]") {
		return "", false
	}
	parsed, err := mail.ParseAddress(raw)
	if err != nil || parsed.Name != "" || parsed.Address != raw {
		return "", false
	}
	local, domain, ok := strings.Cut(parsed.Address, "@")
	if !ok || local == "" || domain == "" {
		return "", false
	}
	return strings.ToLower(parsed.Address), true
}

func combineRecipients(groups ...[]string) []string {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	combined := make([]string, 0, total)
	for _, group := range groups {
		combined = append(combined, group...)
	}
	return combined
}
