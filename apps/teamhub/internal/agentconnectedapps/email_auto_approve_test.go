package agentconnectedapps

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"testing"

	"github.com/simplexai-labs/linger/apps/teamhub/internal/composio"
	"github.com/simplexai-labs/linger/apps/teamhub/internal/externalsource"
)

type scriptedToolProvider struct {
	fakeToolProvider
	tools    map[string]composio.Tool
	handlers map[string]func(map[string]any) (composio.ToolExecution, error)
	executed []string
}

func (provider *scriptedToolProvider) GetTool(_ context.Context, slug, _ string) (composio.Tool, error) {
	if found, ok := provider.tools[slug]; ok {
		return found, nil
	}
	return tool(slug, "outlook"), nil
}

func (provider *scriptedToolProvider) ExecuteTool(_ context.Context, _, _, slug, _ string, arguments map[string]any) (composio.ToolExecution, error) {
	provider.executed = append(provider.executed, slug)
	if handler, ok := provider.handlers[slug]; ok {
		return handler(arguments)
	}
	return composio.ToolExecution{Successful: true, Data: map[string]any{"id": "sent"}}, nil
}

func executeWithAllowlist(t *testing.T, allowlist []string, toolkit, slug string, arguments map[string]any, provider *scriptedToolProvider) (Result, error) {
	t.Helper()
	if provider == nil {
		provider = &scriptedToolProvider{tools: map[string]composio.Tool{slug: tool(slug, toolkit)}}
	}
	connection := externalsource.AgentToolConnection{
		ID: "connection-1", AppKey: toolkit, ToolkitSlug: toolkit, DisplayName: "Mail",
		IdentityLabel: "me@example.test", UpstreamConnectionID: "ca-1", UserID: "owner",
	}
	service := &Service{provider: provider}
	service.SetAutoApproveEmailRecipients(allowlist)
	return service.execute(t.Context(), ExecuteInput{
		ConnectionID: connection.ID, ToolSlug: slug, ToolVersion: tool(slug, toolkit).Version, Arguments: arguments,
	}, func(context.Context) (externalsource.AgentToolConnection, error) { return connection, nil })
}

func TestAutoApproveEmailSendMatchesOnlyAllowlistedRecipients(t *testing.T) {
	allowlist := []string{"me@example.test", "briefings@example.test"}
	cases := []struct {
		name      string
		allowlist []string
		toolkit   string
		slug      string
		arguments map[string]any
		wantRun   bool
	}{
		{
			name:      "outlook bare string recipient allowlisted",
			allowlist: allowlist,
			toolkit:   "outlook",
			slug:      "OUTLOOK_SEND_EMAIL",
			arguments: map[string]any{"to": "Me@Example.TEST", "subject": "Brief", "body": "Hi"},
			wantRun:   true,
		},
		{
			name:      "outlook slice with cc and bcc all allowlisted",
			allowlist: allowlist,
			toolkit:   "outlook",
			slug:      "OUTLOOK_SEND_EMAIL",
			arguments: map[string]any{
				"to":         []any{"me@example.test"},
				"cc_emails":  []any{"Briefings <briefings@example.test>"},
				"bcc_emails": []any{},
				"subject":    "Brief",
				"body":       "Hi",
			},
			wantRun: true,
		},
		{
			name:      "outlook empty allowlist stays confirmed",
			allowlist: nil,
			toolkit:   "outlook",
			slug:      "OUTLOOK_SEND_EMAIL",
			arguments: map[string]any{"to": "me@example.test", "subject": "Brief", "body": "Hi"},
			wantRun:   false,
		},
		{
			name:      "outlook unlisted bcc requires confirmation",
			allowlist: allowlist,
			toolkit:   "outlook",
			slug:      "OUTLOOK_SEND_EMAIL",
			arguments: map[string]any{"to": "me@example.test", "bcc_emails": []any{"other@example.test"}, "subject": "Brief", "body": "Hi"},
			wantRun:   false,
		},
		{
			name:      "outlook comma-separated external recipient requires confirmation",
			allowlist: allowlist,
			toolkit:   "outlook",
			slug:      "OUTLOOK_SEND_EMAIL",
			arguments: map[string]any{"to": "me@example.test, other@example.test", "subject": "Brief", "body": "Hi"},
			wantRun:   false,
		},
		{
			name:      "outlook semicolon-delimited recipients fail closed",
			allowlist: allowlist,
			toolkit:   "outlook",
			slug:      "OUTLOOK_SEND_EMAIL",
			arguments: map[string]any{"to": "me@example.test; briefings@example.test", "subject": "Brief", "body": "Hi"},
			wantRun:   false,
		},
		{
			name:      "outlook crlf injection fails closed",
			allowlist: allowlist,
			toolkit:   "outlook",
			slug:      "OUTLOOK_SEND_EMAIL",
			arguments: map[string]any{"to": "me@example.test\r\nBcc: other@example.test", "subject": "Brief", "body": "Hi"},
			wantRun:   false,
		},
		{
			name:      "outlook unexpected cc key fails closed",
			allowlist: allowlist,
			toolkit:   "outlook",
			slug:      "OUTLOOK_SEND_EMAIL",
			arguments: map[string]any{"to": "me@example.test", "cc": []any{"other@example.test"}, "subject": "Brief", "body": "Hi"},
			wantRun:   false,
		},
		{
			name:      "outlook non-string cc item fails closed",
			allowlist: allowlist,
			toolkit:   "outlook",
			slug:      "OUTLOOK_SEND_EMAIL",
			arguments: map[string]any{"to": "me@example.test", "cc_emails": []any{42}, "subject": "Brief", "body": "Hi"},
			wantRun:   false,
		},
		{
			name:      "gmail send allowlisted across primary extra and cc",
			allowlist: allowlist,
			toolkit:   "gmail",
			slug:      "GMAIL_SEND_EMAIL",
			arguments: map[string]any{
				"recipient_email":  "me@example.test",
				"extra_recipients": []any{"briefings@example.test"},
				"cc":               []any{"me@example.test"},
				"subject":          "Brief",
				"body":             "Hi",
			},
			wantRun: true,
		},
		{
			name:      "gmail send unlisted extra recipient requires confirmation",
			allowlist: allowlist,
			toolkit:   "gmail",
			slug:      "GMAIL_SEND_EMAIL",
			arguments: map[string]any{"recipient_email": "me@example.test", "extra_recipients": []any{"other@example.test"}},
			wantRun:   false,
		},
		{
			name:      "gmail reply allowlisted with explicit primary recipient",
			allowlist: allowlist,
			toolkit:   "gmail",
			slug:      "GMAIL_REPLY_TO_THREAD",
			arguments: map[string]any{"thread_id": "t-1", "recipient_email": "me@example.test", "message_body": "Done"},
			wantRun:   true,
		},
		{
			name:      "gmail reply without primary recipient fails closed",
			allowlist: allowlist,
			toolkit:   "gmail",
			slug:      "GMAIL_REPLY_TO_THREAD",
			arguments: map[string]any{"thread_id": "t-1", "cc": []any{"me@example.test"}, "message_body": "Done"},
			wantRun:   false,
		},
		{
			name:      "non-email write still requires confirmation",
			allowlist: allowlist,
			toolkit:   "slack",
			slug:      "SLACK_SEND_MESSAGE",
			arguments: map[string]any{"channel": "C123", "markdown_text": "me@example.test"},
			wantRun:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &scriptedToolProvider{tools: map[string]composio.Tool{tc.slug: tool(tc.slug, tc.toolkit)}}
			result, err := executeWithAllowlist(t, tc.allowlist, tc.toolkit, tc.slug, tc.arguments, provider)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantRun {
				if result.Confirmation != nil || !slices.Equal(provider.executed, []string{tc.slug}) {
					t.Fatalf("expected immediate send; result=%+v executed=%v", result, provider.executed)
				}
				return
			}
			if result.Confirmation == nil || len(provider.executed) != 0 {
				t.Fatalf("expected confirmation card; result=%+v executed=%v", result, provider.executed)
			}
		})
	}
}

func TestAutoApproveOutlookDraftSendInspectsDraftAndReusesRead(t *testing.T) {
	outlookRecipient := func(name, address string) map[string]any {
		return map[string]any{"emailAddress": map[string]any{"name": name, "address": address}}
	}

	t.Run("runs immediately when every draft recipient is allowlisted", func(t *testing.T) {
		provider := &scriptedToolProvider{
			tools: map[string]composio.Tool{"OUTLOOK_SEND_DRAFT": tool("OUTLOOK_SEND_DRAFT", "outlook")},
			handlers: map[string]func(map[string]any) (composio.ToolExecution, error){
				"OUTLOOK_GET_MESSAGE": func(map[string]any) (composio.ToolExecution, error) {
					return composio.ToolExecution{Successful: true, Data: map[string]any{
						"subject":       "Daily Brief",
						"toRecipients":  []any{outlookRecipient("Me, Self", "Me@Example.TEST")},
						"ccRecipients":  []any{outlookRecipient("Archive", "briefings@example.test")},
						"bccRecipients": []any{},
						"body":          map[string]any{"contentType": "HTML", "content": "<p>Brief</p>"},
					}}, nil
				},
			},
		}
		result, err := executeWithAllowlist(t, []string{"me@example.test", "briefings@example.test"}, "outlook", "OUTLOOK_SEND_DRAFT", map[string]any{"message_id": "draft-1"}, provider)
		if err != nil || result.Confirmation != nil {
			t.Fatalf("expected immediate draft send; result=%+v err=%v", result, err)
		}
		wantCalls := []string{"OUTLOOK_GET_MESSAGE", "OUTLOOK_SEND_DRAFT"}
		if !slices.Equal(provider.executed, wantCalls) {
			t.Fatalf("calls=%v want=%v", provider.executed, wantCalls)
		}
	})

	t.Run("reuses draft read when a recipient is outside the allowlist", func(t *testing.T) {
		provider := &scriptedToolProvider{
			tools: map[string]composio.Tool{"OUTLOOK_SEND_DRAFT": tool("OUTLOOK_SEND_DRAFT", "outlook")},
			handlers: map[string]func(map[string]any) (composio.ToolExecution, error){
				"OUTLOOK_GET_MESSAGE": func(map[string]any) (composio.ToolExecution, error) {
					return composio.ToolExecution{Successful: true, Data: map[string]any{
						"subject":       "Daily Brief",
						"toRecipients":  []any{outlookRecipient("Me", "me@example.test")},
						"bccRecipients": []any{outlookRecipient("External", "external@example.test")},
						"body":          map[string]any{"contentType": "Text", "content": "Brief"},
					}}, nil
				},
				"OUTLOOK_LIST_OUTLOOK_ATTACHMENTS": func(map[string]any) (composio.ToolExecution, error) {
					return composio.ToolExecution{Successful: true, Data: map[string]any{
						"value": []any{map[string]any{"name": "brief.pdf"}},
					}}, nil
				},
			},
		}
		result, err := executeWithAllowlist(t, []string{"me@example.test"}, "outlook", "OUTLOOK_SEND_DRAFT", map[string]any{"message_id": "draft-2"}, provider)
		if err != nil || result.Confirmation == nil {
			t.Fatalf("expected confirmation card; result=%+v err=%v", result, err)
		}
		wantCalls := []string{"OUTLOOK_GET_MESSAGE", "OUTLOOK_LIST_OUTLOOK_ATTACHMENTS"}
		if !slices.Equal(provider.executed, wantCalls) {
			t.Fatalf("calls=%v want=%v", provider.executed, wantCalls)
		}
		if card := result.Confirmation.Card; card.Message == nil || !slices.Equal(card.Message.Attachments, []string{"brief.pdf"}) {
			t.Fatalf("card=%+v", card)
		}
	})

	t.Run("fails closed on non-default user_id or draft read error", func(t *testing.T) {
		provider := &scriptedToolProvider{
			tools: map[string]composio.Tool{"OUTLOOK_SEND_DRAFT": tool("OUTLOOK_SEND_DRAFT", "outlook")},
			handlers: map[string]func(map[string]any) (composio.ToolExecution, error){
				"OUTLOOK_GET_MESSAGE": func(map[string]any) (composio.ToolExecution, error) {
					return composio.ToolExecution{Successful: true, Data: map[string]any{
						"toRecipients": []any{outlookRecipient("Me", "me@example.test")},
					}}, nil
				},
				"OUTLOOK_LIST_OUTLOOK_ATTACHMENTS": func(map[string]any) (composio.ToolExecution, error) {
					return composio.ToolExecution{Successful: true, Data: map[string]any{"value": []any{}}}, nil
				},
			},
		}
		result, err := executeWithAllowlist(t, []string{"me@example.test"}, "outlook", "OUTLOOK_SEND_DRAFT", map[string]any{"message_id": "draft-3", "user_id": "delegate@example.test"}, provider)
		if err != nil || result.Confirmation == nil || slices.Contains(provider.executed, "OUTLOOK_SEND_DRAFT") {
			t.Fatalf("non-default user_id must stay confirmed; result=%+v err=%v calls=%v", result, err, provider.executed)
		}

		failing := &scriptedToolProvider{
			tools: map[string]composio.Tool{"OUTLOOK_SEND_DRAFT": tool("OUTLOOK_SEND_DRAFT", "outlook")},
			handlers: map[string]func(map[string]any) (composio.ToolExecution, error){
				"OUTLOOK_GET_MESSAGE": func(map[string]any) (composio.ToolExecution, error) {
					return composio.ToolExecution{}, errors.New("upstream unavailable")
				},
			},
		}
		if _, err := executeWithAllowlist(t, []string{"me@example.test"}, "outlook", "OUTLOOK_SEND_DRAFT", map[string]any{"message_id": "draft-4"}, failing); err == nil {
			t.Fatal("draft read error must not send or succeed")
		}
		if slices.Contains(failing.executed, "OUTLOOK_SEND_DRAFT") {
			t.Fatalf("draft must not send after read error: %v", failing.executed)
		}
	})
}

func TestAutoApproveGmailDraftSendInspectsAllRecipientHeaders(t *testing.T) {
	body := base64.RawURLEncoding.EncodeToString([]byte("Daily summary"))
	draftResponse := func(headers ...map[string]any) composio.ToolExecution {
		raw := make([]any, len(headers))
		for i, h := range headers {
			raw[i] = h
		}
		return composio.ToolExecution{Successful: true, Data: map[string]any{
			"message": map[string]any{
				"payload": map[string]any{
					"mimeType": "text/plain",
					"headers":  raw,
					"body":     map[string]any{"data": body},
				},
			},
		}}
	}

	t.Run("runs immediately when every header recipient is allowlisted", func(t *testing.T) {
		provider := &scriptedToolProvider{
			tools: map[string]composio.Tool{"GMAIL_SEND_DRAFT": tool("GMAIL_SEND_DRAFT", "gmail")},
			handlers: map[string]func(map[string]any) (composio.ToolExecution, error){
				"GMAIL_GET_DRAFT": func(map[string]any) (composio.ToolExecution, error) {
					return draftResponse(
						map[string]any{"name": "To", "value": `"Doe, Jane" <Me@Example.TEST>, briefings@example.test`},
						map[string]any{"name": "Subject", "value": "Morning brief"},
					), nil
				},
			},
		}
		result, err := executeWithAllowlist(t, []string{"me@example.test", "briefings@example.test"}, "gmail", "GMAIL_SEND_DRAFT", map[string]any{"draft_id": "d-1"}, provider)
		if err != nil || result.Confirmation != nil {
			t.Fatalf("expected immediate gmail draft send; result=%+v err=%v", result, err)
		}
		wantCalls := []string{"GMAIL_GET_DRAFT", "GMAIL_SEND_DRAFT"}
		if !slices.Equal(provider.executed, wantCalls) {
			t.Fatalf("calls=%v want=%v", provider.executed, wantCalls)
		}
	})

	t.Run("reuses draft read and holds when a duplicate To or Resent-To header targets an external address", func(t *testing.T) {
		for name, headers := range map[string][]map[string]any{
			"duplicate To header": {
				{"name": "To", "value": "me@example.test"},
				{"name": "To", "value": "external@example.test"},
			},
			"Resent-To header": {
				{"name": "To", "value": "me@example.test"},
				{"name": "Resent-To", "value": "me@example.test"},
			},
		} {
			t.Run(name, func(t *testing.T) {
				provider := &scriptedToolProvider{
					tools: map[string]composio.Tool{"GMAIL_SEND_DRAFT": tool("GMAIL_SEND_DRAFT", "gmail")},
					handlers: map[string]func(map[string]any) (composio.ToolExecution, error){
						"GMAIL_GET_DRAFT": func(map[string]any) (composio.ToolExecution, error) {
							return draftResponse(headers...), nil
						},
					},
				}
				result, err := executeWithAllowlist(t, []string{"me@example.test"}, "gmail", "GMAIL_SEND_DRAFT", map[string]any{"draft_id": "d-2"}, provider)
				if err != nil || result.Confirmation == nil {
					t.Fatalf("expected confirmation card; result=%+v err=%v", result, err)
				}
				wantCalls := []string{"GMAIL_GET_DRAFT"}
				if !slices.Equal(provider.executed, wantCalls) {
					t.Fatalf("calls=%v want=%v", provider.executed, wantCalls)
				}
			})
		}
	})
}
