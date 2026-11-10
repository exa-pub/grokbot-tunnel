package scenarios

import (
	"google.golang.org/protobuf/encoding/protowire"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	. "github.com/exa-pub/grokbot-tunnel/tests/compare/scenario" //nolint:revive,staticcheck // scenarios read as a list of steps
)

// execs is a scenario of exec requests given as JSON, each a request of its
// own.
func execs(id, about string, messages ...string) Scenario {
	s := Scenario{ID: id, About: about}
	for i, m := range messages {
		s.Steps = append(s.Steps, Do(ExecJSON("r"+string(rune('1'+i)), m)))
	}
	return s
}

func frame(id string, f *aiserverv1.GrokBotUserComputerRequestFrame) Frame {
	f.RequestId = id
	return f
}

func messagesOp(id, opJSON string) Frame {
	return frame(id, &aiserverv1.GrokBotUserComputerRequestFrame{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_MessagesOp{
		MessagesOp: &aiserverv1.GrokBotUserComputerMessagesOp{OpJson: opJSON},
	}})
}

// unknownFrame is a frame of a kind this version of the protocol does not
// have: field 99 of the oneof's message.
func unknownFrame(id string) Frame {
	f := &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: id}
	f.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 99, protowire.BytesType), []byte("x")))
	return f
}

var dispatch = []Scenario{
	execs("dispatch/unsupported", "exec messages the computer has no handler for",
		`{"id":1,"grepArgs":{"pattern":"x"}}`, `{"id":1,"writeArgs":{"path":"a.txt","fileText":"x"}}`,
		`{"id":1,"deleteArgs":{"path":"a.txt"}}`,
		`{"id":1}`, `{}`),
	execs("dispatch/invalid", "exec messages that are not valid",
		`{`, ``, `[]`, `"text"`, `{"id":"x","shellStreamArgs":{"command":"echo a"}}`, `{"id":1,"shellStreamArgs":"echo a"}`,
		`{"id":1,"shellStreamArgs":{"command":5}}`, `{"id":1,"readArgs":{"path":"a","limit":-1}}`),
	execs("dispatch/unknown-fields", "fields and enum values this version does not know",
		`{"id":1,"futureField":true,"shellStreamArgs":{"command":"echo a","futureArg":{"x":1}}}`,
		`{"id":1,"shellStreamArgs":{"command":"echo b","timeoutBehavior":"TIMEOUT_BEHAVIOR_FUTURE"}}`,
		`{"id":1,"shellStreamArgs":{"command":"echo c","timeoutBehavior":7}}`,
		`{"id":1,"shell_stream_args":{"command":"echo d"}}`),
	execs("dispatch/ids", "the IDs of an exec message come back in its answers",
		`{"id":900,"execId":"exec-abc","shellStreamArgs":{"command":"echo a"}}`,
		`{"id":0,"shellStreamArgs":{"command":"echo b"}}`,
		`{"id":4294967295,"execId":"","readArgs":{"path":"missing"}}`,
		`{"execId":"only-exec-id","lsArgs":{"path":"missing"}}`),
	{
		ID: "dispatch/other-frames", About: "frames a Linux computer does not serve",
		Steps: []Step{
			Do(frame("r1", &aiserverv1.GrokBotUserComputerRequestFrame{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_MessagesOp{
				MessagesOp: &aiserverv1.GrokBotUserComputerMessagesOp{OpJson: `{"op":"list"}`},
			}})),
			Do(frame("r2", &aiserverv1.GrokBotUserComputerRequestFrame{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_LocalTool{
				LocalTool: &aiserverv1.GrokBotUserComputerLocalToolCall{ToolId: "computer_use", InputJson: `{}`},
			}})),
			Do(frame("r3", &aiserverv1.GrokBotUserComputerRequestFrame{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_PutFileToUrl{
				PutFileToUrl: &aiserverv1.GrokBotUserComputerPutFileToUrl{Path: "a.txt", Url: "http://127.0.0.1:9/put"},
			}})),
			Do(frame("r4", &aiserverv1.GrokBotUserComputerRequestFrame{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_GetFileFromUrl{
				GetFileFromUrl: &aiserverv1.GrokBotUserComputerGetFileFromUrl{Path: "b.txt", Url: "http://127.0.0.1:9/get", SizeBytes: 1},
			}})),
			// Operations the desktop can parse: a Linux computer has no
			// Messages to run them on.
			Do(messagesOp("r5", `{"kind":"check-permissions"}`)),
			Do(messagesOp("r6", `{"kind":"find-chats","limit":1}`)),
			Do(messagesOp("r7", `{"kind":"send","text":"hi","to":"+10000000000"}`)),
			Do(frame("r8", &aiserverv1.GrokBotUserComputerRequestFrame{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_LocalTool{
				LocalTool: &aiserverv1.GrokBotUserComputerLocalToolCall{ToolId: "", InputJson: `not json`},
			}})),
		},
		Files: []File{Text("a.txt", "x")},
		Watch: []string{"b.txt"},
	},
	{
		ID: "dispatch/empty-frames", About: "frames without a request or of an unknown kind",
		Steps: []Step{
			Send(frame("r1", &aiserverv1.GrokBotUserComputerRequestFrame{})),
			Send(unknownFrame("r2")),
			Send(frame("r3", &aiserverv1.GrokBotUserComputerRequestFrame{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_RetireApproval{
				RetireApproval: &aiserverv1.GrokBotUserComputerRetireApproval{ApprovalId: "approval-1"},
			}})),
			Send(Cancel("r-never-sent")),
			Do(Shell("r5", "echo still-alive")),
		},
	},
	{
		ID: "dispatch/duplicate-id", About: "two requests with one request ID",
		Steps: []Step{Do(Shell("r1", "echo first")), Do(Shell("r1", "echo second"))},
	},
}
