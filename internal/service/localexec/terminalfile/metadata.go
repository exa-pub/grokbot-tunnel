package terminalfile

import (
	"bytes"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

// Metadata parses the envelope of a terminal file, or returns nil if b is not
// one. The desktop's local-exec read and ls never fill TerminalMetadata
// (rnd-v2 Q1); it is kept until the terminal read branch is removed.
func Metadata(b []byte) *agentv1.TerminalMetadata {
	end := HeaderEnd(b)
	if end < 0 {
		return nil
	}
	header := string(b[len(envelopeStart) : end-len(envelopeEnd)])
	m := &agentv1.TerminalMetadata{}
	c := &agentv1.TerminalMetadata_Command{}
	for _, line := range strings.Split(header, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "cwd":
			if v, err := strconv.Unquote(value); err == nil {
				m.Cwd = &v
			}
		case "command":
			if v, err := strconv.Unquote(value); err == nil {
				c.Command = v
			}
		case "started_at":
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
				c.TimestampMs = proto.Int64(t.UnixMilli())
			}
		}
	}
	if code, ok := exitCode(b); ok {
		c.ExitCode = proto.Int32(code)
		m.LastCommands = []*agentv1.TerminalMetadata_Command{c}
	} else {
		m.CurrentCommand = c
	}
	return m
}

func exitCode(b []byte) (int32, bool) {
	footer := bytes.LastIndex(b, []byte(exitMarker))
	if footer < 0 {
		return 0, false
	}
	line, _, _ := strings.Cut(string(b[footer+len(exitMarker):]), "\n")
	code, err := strconv.ParseInt(line, 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(code), true
}
