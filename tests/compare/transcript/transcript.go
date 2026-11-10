// Package transcript is what the comparison tests compare: a canonical record
// of how a client answered the requests of a scenario and what it left on
// disk. Two clients behave the same when their normalized transcripts are
// equal; every difference is either listed in the known differences or a
// failure.
package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// Transcript is the record of one client running one scenario.
type Transcript struct {
	Scenario string `json:"scenario"`
	// Digest identifies the scenario's requests and fixtures; a recording
	// with another digest is stale.
	Digest  string `json:"digest"`
	Subject string `json:"subject"`
	// Env names the programs the client ran with. It is not compared.
	Env map[string]string `json:"env,omitempty"`
	// Hello is the hello of the watch stream, for scenarios that compare it.
	Hello any `json:"hello,omitempty"`
	// Requests maps a request ID to the frames that answered it, in order.
	Requests map[string][]any `json:"requests"`
	// Files maps a watched path, relative to the root, to its state after
	// the scenario.
	Files map[string]any `json:"files,omitempty"`
	// Calls are the channel calls the client made, for scenarios that
	// compare them.
	Calls []any `json:"calls,omitempty"`
	// Headers maps a kind of channel call to the HTTP headers of its first
	// call, for scenarios that compare them.
	Headers map[string]any `json:"headers,omitempty"`
}

// Compared returns the part of t that takes part in the comparison, as a
// JSON value.
func (t *Transcript) Compared() any {
	return generic(map[string]any{
		"hello": t.Hello, "requests": t.Requests, "files": t.Files, "calls": t.Calls, "headers": t.Headers,
	})
}

// Frame converts a response frame to its transcript form: an object with one
// key naming the frame. Exec messages are parsed, since the order of their
// JSON keys and the fields left at their defaults are not behavior.
func Frame(f *aiserverv1.GrokBotUserComputerResponseFrame) map[string]any {
	switch {
	case f.GetClient() != nil:
		return map[string]any{"client": execMessage(f.GetClient().GetMessageJson(), &agentv1.ExecClientMessage{})}
	case f.GetControl() != nil:
		out := map[string]any{"control": execMessage(f.GetControl().GetMessageJson(), &agentv1.ExecClientControlMessage{})}
		if f.GetControl().CwdState != nil {
			out["cwdState"] = f.GetControl().GetCwdState()
		}
		return out
	case f.GetFile() != nil:
		file := Blob(f.GetFile().GetData())
		file["seq"] = json.Number(fmt.Sprint(f.GetFile().GetSeq()))
		file["last"] = f.GetFile().GetLast()
		return map[string]any{"file": file}
	}
	clone := &aiserverv1.GrokBotUserComputerResponseFrame{}
	b, err := protojson.Marshal(f)
	if err == nil {
		err = protojson.Unmarshal(b, clone)
	}
	if err != nil {
		return map[string]any{"invalid": err.Error()}
	}
	clone.RequestId = ""
	b, err = protojson.Marshal(clone)
	if err != nil {
		return map[string]any{"invalid": err.Error()}
	}
	out, ok := parse(b).(map[string]any)
	if !ok {
		return map[string]any{"invalid": string(b)}
	}
	if unknown := f.ProtoReflect().GetUnknown(); len(unknown) > 0 {
		out["unknownFields"] = hex.EncodeToString(unknown)
	}
	return out
}

// Blob describes bytes by their size and hash, and by their text when they
// are short valid UTF-8 or a terminal file, whose size and hash change with
// every run.
func Blob(data []byte) map[string]any {
	sum := sha256.Sum256(data)
	out := map[string]any{"size": json.Number(fmt.Sprint(len(data))), "sha256": hex.EncodeToString(sum[:])}
	if utf8.Valid(data) && (len(data) < longText || bytes.HasPrefix(data, []byte(terminalHeader))) {
		out["text"] = string(data)
	}
	return out
}

// parse decodes JSON keeping numbers exact; nil if b is not JSON.
func parse(b []byte) any {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil || d.More() {
		return nil
	}
	return v
}

// generic converts a value built from Go maps and slices to the form JSON
// decoding gives, so that values compare regardless of how they were built.
func generic(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // transcripts hold only JSON values
	}
	return parse(b)
}
