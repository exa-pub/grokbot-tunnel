package read_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/read"
)

func newReader(t *testing.T) (*read.Executor, *pathroot.Root) {
	t.Helper()
	root, err := pathroot.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { root.Close() })
	return read.New(root), root
}

// readText reads text through the executor with the reference read
// arguments.
func readText(t *testing.T, rd *read.Executor, root *pathroot.Root, text string, args json.RawMessage) *agentv1.ReadSuccess {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root.Dir(), "reference"), []byte(text), 0o600))
	x := &agentv1.ReadArgs{}
	require.NoError(t, protojson.Unmarshal(args, x))
	x.Path = "reference"
	var got *agentv1.ExecClientMessage
	require.NoError(t, rd.Execute(context.Background(), x, func(m *agentv1.ExecClientMessage) error { got = m; return nil }))
	return got.GetReadResult().GetSuccess()
}

// Line ranges match the desktop (testdata/compat, "ranges").
func TestReferenceRanges(t *testing.T) {
	ref := compattest.Load(t)
	rd, root := newReader(t)
	for _, tc := range ref.Ranges {
		got := readText(t, rd, root, tc.Text, tc.Args)
		require.NotNil(t, got, tc.Args)
		assert.Equal(t, tc.Result.Content, got.GetContent(), string(tc.Args))
		assert.Equal(t, tc.Result.RangeApplied, got.RangeApplied, string(tc.Args))
	}
}

// The desktop's streaming reader, including max_chars, and the decoding of
// invalid UTF-8 (testdata/compat, "streaming" and "fallback").
func TestReferenceStreamingReader(t *testing.T) {
	ref := compattest.Load(t)
	require.NotEmpty(t, ref.Streaming)
	require.NotEmpty(t, ref.Fallback)
	rd, root := newReader(t)
	for _, tc := range ref.Streaming {
		text, want := tc.Input(), tc.Content()
		got := readText(t, rd, root, text, tc.Args)
		require.NotNil(t, got, string(tc.Args))
		if got.GetContent() != want || got.TotalLines != tc.Result.TotalLines || got.Truncated != tc.Result.Truncated ||
			got.RangeApplied != tc.Result.RangeApplied {
			t.Errorf("streaming %.40q %s: got %d units truncated=%v lines=%d range=%v, want %d units %+v",
				text, tc.Args, textcodec.UTF16Len(got.GetContent()), got.GetTruncated(), got.GetTotalLines(), got.GetRangeApplied(),
				textcodec.UTF16Len(want), tc.Result.Truncated)
		}
	}
	for _, tc := range ref.Fallback {
		raw, err := hex.DecodeString(tc.Hex)
		require.NoError(t, err)
		want := tc.Lossy
		if !tc.MostlyUtf8 {
			out := make([]rune, len(raw))
			for i, c := range raw {
				out[i] = rune(c) // Latin-1
			}
			want = string(out)
		}
		assert.Equal(t, want, readText(t, rd, root, string(raw), json.RawMessage(`{}`)).GetContent(), tc.Hex)
	}
}
