package scenario_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/tests/compare/scenario"
	"github.com/exa-pub/grokbot-tunnel/tests/compare/scenarios"
)

// The digest of a scenario is the same on every run and changes with
// anything that decides the answers.
func TestDigest(t *testing.T) {
	build := func(command string) scenario.Scenario {
		return scenario.Scenario{
			ID: "x/y", Files: []scenario.File{scenario.Text("a.txt", "a")},
			Steps: []scenario.Step{scenario.Do(scenario.Shell("r1", command))},
		}
	}
	base := build("echo a")
	assert.Equal(t, base.Digest(), build("echo a").Digest())
	assert.NotEqual(t, base.Digest(), build("echo b").Digest())

	other := build("echo a")
	other.Files[0].Data = []byte("b")
	assert.NotEqual(t, base.Digest(), other.Digest())
	other = build("echo a")
	other.Permission = "never"
	assert.NotEqual(t, base.Digest(), other.Digest())
	// What a scenario is called is not part of it.
	other = build("echo a")
	other.ID, other.About = "x/z", "renamed"
	assert.Equal(t, base.Digest(), other.Digest())
}

// Every scenario has a digest, and no two scenarios are the same one under
// two names.
func TestScenariosAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, s := range scenarios.All() {
		require.NotEmpty(t, s.Steps, s.ID)
		digest := s.Digest()
		assert.Empty(t, seen[digest], "%s repeats %s", s.ID, seen[digest])
		seen[digest] = s.ID
	}
}

func control(messageJSON string) *aiserverv1.GrokBotUserComputerResponseFrame {
	return &aiserverv1.GrokBotUserComputerResponseFrame{Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Control{
		Control: &aiserverv1.GrokBotUserComputerControlMessage{MessageJson: messageJSON},
	}}
}

func file(last bool) *aiserverv1.GrokBotUserComputerResponseFrame {
	return &aiserverv1.GrokBotUserComputerResponseFrame{Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{
		File: &aiserverv1.GrokBotUserComputerFile{Last: last},
	}}
}

func TestAnswered(t *testing.T) {
	type frames = []*aiserverv1.GrokBotUserComputerResponseFrame
	client := &aiserverv1.GrokBotUserComputerResponseFrame{Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
		Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: `{"shellStream":{"exit":{}}}`},
	}}
	alive := &aiserverv1.GrokBotUserComputerResponseFrame{Frame: &aiserverv1.GrokBotUserComputerResponseFrame_MessagesAccepted{
		MessagesAccepted: &aiserverv1.GrokBotUserComputerMessagesAccepted{},
	}}
	refused := &aiserverv1.GrokBotUserComputerResponseFrame{Frame: &aiserverv1.GrokBotUserComputerResponseFrame_FileError{
		FileError: &aiserverv1.GrokBotUserComputerFileError{Error: "no"},
	}}

	assert.False(t, scenario.Answered(nil))
	assert.False(t, scenario.Answered(frames{client, alive, control(`{"heartbeat":{"id":1}}`), file(false)}))
	assert.True(t, scenario.Answered(frames{client, control(`{"streamClose":{"id":1}}`)}))
	assert.True(t, scenario.Answered(frames{control(`{"throw":{"error":"no"}}`)}))
	assert.True(t, scenario.Answered(frames{file(false), file(true)}))
	assert.True(t, scenario.Answered(frames{refused}))
}

func TestExpand(t *testing.T) {
	dirs := map[string]string{"$ROOT": "/w/root", "$HOME": "/w/home"}
	read := scenario.Expand(scenario.Read("r1", "$ROOT/a.txt"), dirs)
	assert.Contains(t, read.GetExec().GetServerMessageJson(), `"/w/root/a.txt"`)
	assert.Equal(t, "/w/home/b", scenario.Expand(scenario.Upload("r2", "$HOME/b", nil), dirs).GetUpload().GetPath())
	assert.Equal(t, "/w/root", scenario.Expand(scenario.Download("r3", "$ROOT"), dirs).GetDownload().GetPath())
	// The scenario's own frame stays as written.
	original := scenario.Download("r4", "$ROOT/x")
	scenario.Expand(original, dirs)
	assert.Equal(t, "$ROOT/x", original.GetDownload().GetPath())
}

func TestWithAuthorization(t *testing.T) {
	plain := scenario.Shell("r1", "true")
	marked := scenario.With(plain, scenario.Authorization{ApprovalID: "a1", ByApproval: true})
	assert.Equal(t, "a1", marked.GetExec().GetApprovalId())
	assert.True(t, marked.GetExec().GetAuthorizedByApproval())
	assert.False(t, marked.GetExec().GetAuthorizedByStanding())
	assert.Nil(t, plain.GetExec().ApprovalId)
	assert.True(t, scenario.With(scenario.Download("r2", "a"), scenario.Authorization{ByStanding: true}).GetDownload().GetAuthorizedByStanding())
}

func TestCreate(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, scenario.Create(dir, []scenario.File{
		scenario.Text("a/b/c.txt", "c").WithMode(0o600), scenario.Dir("empty"), scenario.Link("link", "$DIR/a"),
		scenario.Seq("seq.txt", 3), scenario.Sparse("sparse.bin", 4096), {Path: "rep.txt", Data: []byte("ab"), Repeat: 3},
	}))
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	assert.Equal(t, "c", read("a/b/c.txt"))
	assert.Equal(t, "1\n2\n3\n", read("seq.txt"))
	assert.Equal(t, "ababab", read("rep.txt"))
	info, err := os.Stat(filepath.Join(dir, "a/b/c.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	// Every fixture and the directories made for it get the same time, so
	// that answers with times do not depend on when the test ran.
	assert.True(t, info.ModTime().Equal(scenario.Created))
	info, err = os.Stat(filepath.Join(dir, "a"))
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(scenario.Created))
	info, err = os.Stat(filepath.Join(dir, "sparse.bin"))
	require.NoError(t, err)
	assert.Equal(t, int64(4096), info.Size())
	target, err := os.Readlink(filepath.Join(dir, "link"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "a"), target)

	require.NoError(t, scenario.Create(dir, []scenario.File{{Path: "a", Remove: true}}))
	assert.NoFileExists(t, filepath.Join(dir, "a/b/c.txt"))
	require.Error(t, scenario.Create(dir, []scenario.File{scenario.Text("../escape", "x")}))
}
