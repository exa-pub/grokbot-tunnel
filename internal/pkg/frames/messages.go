package frames

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// ErrMessagesOff answers a Messages operation in the desktop's words for a
// computer whose Messages are turned off, as they are on every Linux
// computer: Messages are a macOS feature.
//
//nolint:revive,staticcheck // user-facing sentence
var ErrMessagesOff = errors.New("Messages is turned off in the user's settings, so their Messages cannot be read or sent from here. " +
	"Do not retry while it stays off. If the user wants this, ask them to turn it back on in " +
	"[Messages](grokbot://app/v1/messages) (Connect Apps → Mac → Messages).")

// NoLocalTool answers a call of a local tool the computer does not have, in
// the desktop's words. A Linux computer announces none.
func NoLocalTool(toolID string) error {
	return errors.New(`This computer has no "` + toolID + `" tool.`)
}

// MessagesOff answers a Messages operation the desktop can parse (see
// messagesOp): a send is refused as a consent verdict, anything else as an
// error.
func MessagesOff(f *aiserverv1.GrokBotUserComputerRequestFrame, sink Sink) error {
	if kind, _ := messagesOp(f.GetMessagesOp().GetOpJson()); kind != "send" {
		return Reject(f, ErrMessagesOff, sink)
	}
	reason := ErrMessagesOff.Error()
	return sink(&aiserverv1.GrokBotUserComputerResponseFrame{
		RequestId: f.RequestId,
		Frame: &aiserverv1.GrokBotUserComputerResponseFrame_MessagesConsentResult{
			MessagesConsentResult: &aiserverv1.GrokBotUserComputerMessagesConsentResult{
				Verdict: aiserverv1.GrokBotUserComputerMessagesConsentVerdict_GROK_BOT_USER_COMPUTER_MESSAGES_CONSENT_VERDICT_REFUSED,
				Reason:  &reason,
			},
		},
	})
}

// opField is a field of a Messages operation.
type opField struct {
	name     string
	required bool
	valid    func(json.RawMessage) bool
}

// messagesKinds are the kinds of Messages operations and their fields.
var messagesKinds = map[string][]opField{
	"find-chats":        {{"displayName", false, isString}, {"handle", false, isString}, {"limit", false, isLimit}},
	"items":             {{"chatGuid", false, isString}, {"limit", false, isLimit}, {"before", false, isCursor}},
	"search":            {{"needle", true, isString}, {"limit", false, isLimit}, {"before", false, isCursor}},
	"activity":          {{"since", true, isTime}, {"until", false, isTime}},
	"send":              {{"text", true, isString}, {"service", false, isService}, {"to", false, isString}, {"chatId", false, isString}},
	"check-permissions": {},
	"fetch-attachment":  {{"messageGuid", true, isString}, {"attachmentGuid", true, isString}},
	"find-contacts":     {{"query", true, isNotBlank}},
}

// messagesOp returns the kind of a Messages operation and whether the
// desktop parses it: an object of one of its kinds with the fields of that
// kind and no others. Anything else is a frame the desktop does not
// understand (ErrUnknownFrame).
func messagesOp(opJSON string) (kind string, ok bool) {
	var op map[string]json.RawMessage
	if json.Unmarshal([]byte(opJSON), &op) != nil || json.Unmarshal(op["kind"], &kind) != nil {
		return "", false
	}
	fields, known := messagesKinds[kind]
	if !known {
		return kind, false
	}
	// A send names its recipient by one of "to" and "chatId", never both.
	_, to := op["to"]
	_, chat := op["chatId"]
	if kind == "send" && to == chat {
		return kind, false
	}
	delete(op, "kind")
	for _, f := range fields {
		raw, present := op[f.name]
		delete(op, f.name)
		if present && !f.valid(raw) || !present && f.required {
			return kind, false
		}
	}
	// What is left is a field the kind does not have.
	return kind, len(op) == 0
}

func isNotBlank(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != ""
}

func isString(raw json.RawMessage) bool {
	var s *string
	return json.Unmarshal(raw, &s) == nil && s != nil
}

// isInt accepts what JavaScript calls an integer: 5 and 5.0, not 5.5.
func isInt(raw json.RawMessage) (float64, bool) {
	var n *float64
	if json.Unmarshal(raw, &n) != nil || n == nil {
		return 0, false
	}
	return *n, *n == math.Trunc(*n) && !math.IsInf(*n, 0)
}

func isLimit(raw json.RawMessage) bool {
	n, ok := isInt(raw)
	return ok && n >= 1
}

func isService(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && (s == "iMessage" || s == "SMS" || s == "auto")
}

// isTime accepts an ISO date and time with a zone, "Z" or an offset.
func isTime(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

// isCursor accepts the position a listing continues before: {date, id}.
func isCursor(raw json.RawMessage) bool {
	var cursor map[string]json.RawMessage
	if json.Unmarshal(raw, &cursor) != nil || len(cursor) != 2 {
		return false
	}
	date, hasDate := cursor["date"]
	id, hasID := cursor["id"]
	if !hasDate || !hasID {
		return false
	}
	_, isID := isInt(id)
	return isTime(date) && isID
}
