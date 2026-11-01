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

func TestRegister(t *testing.T) {
	d := &dashboard{}
	path, h := rpc.NewDashboardServiceHandler(d)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := rpc.NewDashboardServiceClient(srv.Client(), srv.URL)

	require.NoError(t, computer.Register(context.Background(), client, "box", entity.ModeAsk))
	assert.Equal(t, "box", d.got.GetLabel())
	assert.Equal(t, "ask", d.got.GetLocalToolPermission())

	d.fail = true
	err := computer.Register(context.Background(), client, "box", entity.ModeAsk)
	require.ErrorContains(t, err, "register computer: ")
	assert.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
}
