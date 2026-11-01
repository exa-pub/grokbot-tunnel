package computer

import (
	"os"
	"strings"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// defaultLabel names the computer in the hello when it has no label and the
// host name is unknown, in the desktop's words.
const defaultLabel = "this computer"

// hostname is os.Hostname; tests replace it.
var hostname = os.Hostname

// hello describes this computer at the start of a watch stream. In
// server-authoritative mode the server decides, so no standing is sent.
// The variant is the desktop's for its production backend.
func (pv *Provider) hello(c entity.Credential, standing string) *aiserverv1.GrokBotUserComputerHello {
	if c.ServerAuthoritative {
		standing = ""
	}
	setup := true
	generation := int32(4)
	return &aiserverv1.GrokBotUserComputerHello{
		Label:               pv.label(),
		LocalRoot:           pv.LocalRoot,
		TerminalsFolder:     pv.TerminalsFolder,
		Standing:            standing,
		Variant:             "sand",
		ServerAuthoritative: c.ServerAuthoritative,
		Capabilities:        &aiserverv1.GrokBotUserComputerCapabilities{AiToolSetup: &setup, MessagesOpGeneration: &generation},
	}
}

// label is the computer's label in the hello, like the desktop's
// computerLabel: the machine's label, else the host name, trimmed, else
// defaultLabel.
func (pv *Provider) label() string {
	if label := strings.TrimSpace(pv.Machine.Label); label != "" {
		return label
	}
	if host, err := hostname(); err == nil {
		if host = strings.TrimSpace(host); host != "" {
			return host
		}
	}
	return defaultLabel
}

// standing is the standing mode announced in hello; the server decides in
// server-authoritative mode, so none is sent.
func (pv *Provider) standing(c entity.Credential) string {
	if c.ServerAuthoritative || pv.Standing == nil {
		return ""
	}
	return string(pv.Standing())
}
