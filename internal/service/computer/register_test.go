package computer_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer"
)

type dashboard struct {
	rpc.UnimplementedDashboardServiceHandler
	got  *aiserverv1.RegisterSandMachineRequest
	fail bool
	// mode is the privacy mode the server answers with, unless noMode;
	// privacy is the last request for it.
	mode    aiserverv1.PrivacyMode
	noMode  bool
	privacy *aiserverv1.GetUserPrivacyModeRequest
	// update is the last permission update; kept, if set, the mode the
	// server keeps instead of the one asked for.
	update *aiserverv1.UpdateSandMachineLocalToolPermissionRequest
	kept   string
}

func (d *dashboard) UpdateSandMachineLocalToolPermission(
	_ context.Context, r *connect.Request[aiserverv1.UpdateSandMachineLocalToolPermissionRequest],
) (*connect.Response[aiserverv1.UpdateSandMachineLocalToolPermissionResponse], error) {
	d.update = r.Msg
	if d.fail {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("storage off"))
	}
	kept := r.Msg.GetLocalToolPermission()
	if d.kept != "" {
		kept = d.kept
	}
	return connect.NewResponse(&aiserverv1.UpdateSandMachineLocalToolPermissionResponse{
		Machine: &aiserverv1.SandMachine{MachineId: r.Msg.GetMachineId(), LocalToolPermission: &kept},
	}), nil
}

func (d *dashboard) RegisterSandMachine(
	_ context.Context, r *connect.Request[aiserverv1.RegisterSandMachineRequest],
) (*connect.Response[aiserverv1.RegisterSandMachineResponse], error) {
	d.got = r.Msg
	if d.fail {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	return connect.NewResponse(&aiserverv1.RegisterSandMachineResponse{}), nil
}

func (d *dashboard) GetUserPrivacyMode(
	_ context.Context, r *connect.Request[aiserverv1.GetUserPrivacyModeRequest],
) (*connect.Response[aiserverv1.GetUserPrivacyModeResponse], error) {
	d.privacy = r.Msg
	if d.noMode {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	return connect.NewResponse(&aiserverv1.GetUserPrivacyModeResponse{PrivacyMode: d.mode}), nil
}

func TestRegister(t *testing.T) {
	d := &dashboard{mode: aiserverv1.PrivacyMode_PRIVACY_MODE_NO_TRAINING}
	path, h := rpc.NewDashboardServiceHandler(d)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := rpc.NewDashboardServiceClient(srv.Client(), srv.URL)

	registered, err := computer.Register(context.Background(), client, "box", entity.ModeAsk, "7")
	require.NoError(t, err)
	assert.True(t, registered)
	assert.Equal(t, "box", d.got.GetLabel())
	assert.Equal(t, "ask", d.got.GetLocalToolPermission())
	assert.Equal(t, int32(7), d.privacy.GetTeamId())

	d.fail = true
	_, err = computer.Register(context.Background(), client, "box", entity.ModeAsk, "7")
	require.ErrorContains(t, err, "register computer: ")
	assert.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
}

// Like the desktop, a computer is registered only if the privacy mode of the
// selected team allows storage; a team is named only by a positive ID.
func TestRegisterPrivacy(t *testing.T) {
	d := &dashboard{}
	path, h := rpc.NewDashboardServiceHandler(d)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := rpc.NewDashboardServiceClient(srv.Client(), srv.URL)

	for mode, want := range map[aiserverv1.PrivacyMode]bool{
		aiserverv1.PrivacyMode_PRIVACY_MODE_UNSPECIFIED:                     false,
		aiserverv1.PrivacyMode_PRIVACY_MODE_NO_STORAGE:                      false,
		aiserverv1.PrivacyMode_PRIVACY_MODE_NO_TRAINING:                     true,
		aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_DATA_TRAINING_ALLOWED:     true,
		aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_CODEBASE_TRAINING_ALLOWED: true,
	} {
		d.mode, d.got = mode, nil
		registered, err := computer.Register(context.Background(), client, "box", entity.ModeAsk, "")
		require.NoError(t, err, mode)
		assert.Equal(t, want, registered, mode)
		assert.Equal(t, want, d.got != nil, mode)
	}
	for _, team := range []string{"", "0", "-1", "x"} {
		_, err := computer.Register(context.Background(), client, "box", entity.ModeAsk, team)
		require.NoError(t, err)
		assert.Nil(t, d.privacy.TeamId, team)
	}

	d.noMode, d.got = true, nil
	_, err := computer.Register(context.Background(), client, "box", entity.ModeAsk, "")
	require.ErrorContains(t, err, "check the account's privacy mode: ")
	assert.Nil(t, d.got)
}

// The permission set with --permission becomes the computer's in Grok Bot,
// like the desktop's update of a machine's permission; the server may keep
// a lower one, e.g. by a team ceiling.
func TestSetPermission(t *testing.T) {
	d := &dashboard{}
	path, h := rpc.NewDashboardServiceHandler(d)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := rpc.NewDashboardServiceClient(srv.Client(), srv.URL)

	kept, err := computer.SetPermission(context.Background(), client, "m1", entity.ModeNever)
	require.NoError(t, err)
	assert.Equal(t, entity.ModeNever, kept)
	assert.Equal(t, "m1", d.update.GetMachineId())
	assert.Equal(t, "never", d.update.GetLocalToolPermission())

	d.kept = "ask"
	kept, err = computer.SetPermission(context.Background(), client, "m1", entity.ModeAlways)
	require.NoError(t, err)
	assert.Equal(t, entity.ModeAsk, kept)

	d.kept = "sometimes"
	_, err = computer.SetPermission(context.Background(), client, "m1", entity.ModeAlways)
	require.ErrorContains(t, err, `unexpected mode "sometimes"`)

	d.fail = true
	_, err = computer.SetPermission(context.Background(), client, "m1", entity.ModeAlways)
	require.ErrorContains(t, err, "set the computer's permission in Grok Bot: ")
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
}
