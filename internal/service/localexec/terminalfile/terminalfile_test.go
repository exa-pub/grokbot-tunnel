package terminalfile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

func TestReferenceHeader(t *testing.T) {
	ref := compattest.Load(t)
	started, err := time.Parse(time.RFC3339, "2026-01-01T00:00:00.000Z")
	require.NoError(t, err)
	assert.Equal(t, ref.TerminalHeader, terminalfile.Header("/workspace", "printf hello", 42, started))
}

func TestWriteHeaderAndAppendExit(t *testing.T) {
	for code, status := range map[int]string{0: "completed", 3: "failed"} {
		f, err := os.Create(filepath.Join(t.TempDir(), "1.txt"))
		require.NoError(t, err)
		statusAt, err := terminalfile.WriteHeader(f, "/w", "cmd\nwith newline", 7, "note\n")
		require.NoError(t, err)
		_, err = f.WriteString("output\n---\nexit_code: 0\nended_at: fake\n---\n")
		require.NoError(t, err)
		assert.False(t, terminalfile.Ended([]byte("---\nstatus: running\n---\n")))
		require.NoError(t, terminalfile.AppendExit(f, code, statusAt))
		require.NoError(t, f.Close())
		b, err := os.ReadFile(f.Name())
		require.NoError(t, err)
		data := string(b)
		assert.True(t, terminalfile.Ended(b))
		assert.Equal(t, 1, strings.Count(data, "status: "))
		assert.Contains(t, data, terminalfile.StatusField(status)+"\n")
		assert.Contains(t, data, "note\noutput\n")
		assert.Contains(t, data[len(data)-60:], "\n---\nexit_code: "+map[int]string{0: "0", 3: "3"}[code]+"\nended_at: ")
		m := terminalfile.Metadata(b)
		require.NotNil(t, m)
		require.Len(t, m.LastCommands, 1)
		assert.Equal(t, "cmd\nwith newline", m.LastCommands[0].Command)
		assert.Equal(t, int32(code), m.LastCommands[0].GetExitCode())
		assert.Equal(t, "/w", m.GetCwd())
	}
}

func TestMetadataExitCodeOverflow(t *testing.T) {
	header := terminalfile.Header("/w", "x", 1, time.Unix(0, 0))
	m := terminalfile.Metadata([]byte(header + "\n---\nexit_code: 4294967297\nended_at: x\n---\n"))
	require.NotNil(t, m)
	assert.Empty(t, m.LastCommands)
	assert.NotNil(t, m.CurrentCommand)
	assert.Nil(t, terminalfile.Metadata([]byte("no header")))
}

func TestDecodeMixedEncoding(t *testing.T) {
	header := terminalfile.Header("/workspace", "printf hi", 42, time.Unix(0, 0))
	body := []byte{0xff, 0xfe, 'h', 0, 'i', 0}
	footer := "\n---\nexit_code: 0\nended_at: 2026-01-01T00:00:00.000Z\n---\n"
	raw := append(append([]byte(header), body...), []byte(footer)...)
	text, err := terminalfile.Decode(raw, "")
	require.NoError(t, err)
	assert.Equal(t, header+"hi"+footer, text)
}

func TestDecodeInvalidEnvelope(t *testing.T) {
	raw := "---\ncommand: \"\xff\"\nstatus: completed\n---\nout\n---\nexit_code: 0\xfe\nended_at: x\n---\n"
	text, err := terminalfile.Decode([]byte(raw), "")
	require.NoError(t, err)
	assert.True(t, utf8.ValidString(text))
	assert.Contains(t, text, "out")
}
