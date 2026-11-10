package frames_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
)

func messagesOp(opJSON string) *request {
	return &request{RequestId: "m", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_MessagesOp{
		MessagesOp: &aiserverv1.GrokBotUserComputerMessagesOp{OpJson: opJSON},
	}}
}

// A Messages operation is one the desktop parses only if it has the fields
// of its kind and no others; anything else is a frame the desktop does not
// understand.
func TestMessagesOperations(t *testing.T) {
	for _, op := range []string{
		`{"kind":"check-permissions"}`,
		`{"kind":"find-chats"}`, `{"kind":"find-chats","displayName":"a","handle":"b","limit":5}`, `{"kind":"find-chats","limit":2.0}`,
		`{"kind":"items","chatGuid":"g","before":{"date":"2026-01-02T03:04:05Z","id":7}}`,
		`{"kind":"search","needle":""}`,
		`{"kind":"activity","since":"2026-01-02T03:04:05.123+02:00"}`, `{"kind":"activity","since":"2026-01-02T03:04:05Z","until":"2026-01-03T00:00:00Z"}`,
		`{"kind":"send","text":"hi","to":"+1"}`, `{"kind":"send","text":"hi","chatId":"c","service":"SMS"}`,
		`{"kind":"fetch-attachment","messageGuid":"m","attachmentGuid":"a"}`,
		`{"kind":"find-contacts","query":" x "}`,
	} {
		assert.False(t, frames.Unrecognized(messagesOp(op)), op)
	}
	for _, op := range []string{
		``, `{`, `[]`, `null`, `"send"`, `{}`, `{"kind":5}`, `{"kind":"list"}`, `{"op":"check-permissions"}`,
		`{"kind":"check-permissions","extra":1}`,
		`{"kind":"find-chats","limit":0}`, `{"kind":"find-chats","limit":1.5}`, `{"kind":"find-chats","limit":"1"}`,
		`{"kind":"find-chats","handle":null}`,
		`{"kind":"items","before":{"date":"2026-01-02T03:04:05Z"}}`, `{"kind":"items","before":{"date":"yesterday","id":1}}`,
		`{"kind":"items","before":{"date":"2026-01-02T03:04:05Z","id":1,"x":1}}`,
		`{"kind":"search"}`, `{"kind":"search","needle":5}`,
		`{"kind":"activity"}`, `{"kind":"activity","since":"2026-01-02T03:04:05"}`, `{"kind":"activity","since":"2026-01-02"}`,
		`{"kind":"send","text":"hi"}`, `{"kind":"send","to":"+1"}`, `{"kind":"send","text":"hi","to":"+1","chatId":"c"}`,
		`{"kind":"send","text":"hi","to":"+1","service":"fax"}`,
		`{"kind":"fetch-attachment","messageGuid":"m"}`,
		`{"kind":"find-contacts","query":"  "}`, `{"kind":"find-contacts"}`,
	} {
		assert.True(t, frames.Unrecognized(messagesOp(op)), op)
	}
}

// An operation the desktop parses is answered as by a computer whose
// Messages are off: a send with a refused consent, others with an error.
func TestMessagesOff(t *testing.T) {
	var got []*response
	sink := func(f *response) error { got = append(got, f); return nil }
	require.NoError(t, frames.MessagesOff(messagesOp(`{"kind":"find-chats"}`), sink))
	require.NoError(t, frames.MessagesOff(messagesOp(`{"kind":"send","text":"hi","to":"+1"}`), sink))
	require.Len(t, got, 2)
	assert.Equal(t, "m", got[0].RequestId)
	assert.Equal(t, frames.ErrMessagesOff.Error(), got[0].GetMessagesError().GetError())
	consent := got[1].GetMessagesConsentResult()
	require.NotNil(t, consent)
	assert.Equal(t, aiserverv1.GrokBotUserComputerMessagesConsentVerdict_GROK_BOT_USER_COMPUTER_MESSAGES_CONSENT_VERDICT_REFUSED, consent.GetVerdict())
	assert.Equal(t, frames.ErrMessagesOff.Error(), consent.GetReason())
}

func TestNoLocalTool(t *testing.T) {
	require.EqualError(t, frames.NoLocalTool("computer_use"), `This computer has no "computer_use" tool.`)
	require.EqualError(t, frames.NoLocalTool(""), `This computer has no "" tool.`)
}
