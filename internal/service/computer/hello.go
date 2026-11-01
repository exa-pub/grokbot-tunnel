package computer

import (
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// hello describes this computer at the start of a watch stream. In
// server-authoritative mode the server decides, so no standing is sent.
func (pv *Provider) hello(c entity.Credential, standing string) *aiserverv1.GrokBotUserComputerHello {
	if c.ServerAuthoritative {
		standing = ""
	}
	setup := true
	generation := int32(4)
	return &aiserverv1.GrokBotUserComputerHello{
		Label:               pv.Machine.Label,
		LocalRoot:           pv.LocalRoot,
		TerminalsFolder:     pv.TerminalsFolder,
		Standing:            standing,
		Variant:             "sand",
		ServerAuthoritative: c.ServerAuthoritative,
		Capabilities:        &aiserverv1.GrokBotUserComputerCapabilities{AiToolSetup: &setup, MessagesOpGeneration: &generation},
	}
}

// standing is the standing mode announced in hello; the server decides in
// server-authoritative mode, so none is sent.
func (pv *Provider) standing(c entity.Credential) string {
	if c.ServerAuthoritative || pv.Standing == nil {
		return ""
	}
	return string(pv.Standing())
}
