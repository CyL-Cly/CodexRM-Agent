package agent

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/skip2/go-qrcode"
)

// PairingLink keeps the HTTP invitation out of any browser URL. The App
// scanner recognizes the CodexRM scheme and reads the relay address locally.
func (a *Agent) PairingLink(code string) (qrLink, webLink string) {
	if a.cfg.SecureRelay && strings.HasPrefix(a.cfg.ServerURL, "http://") {
		params := url.Values{"server": {a.cfg.ServerURL}, "code": {code}}
		return "codexrm://pair?" + params.Encode(), ""
	}
	link := a.cfg.ServerURL + "/#pair=" + url.QueryEscape(code)
	return link, link
}

func (a *Agent) PairingInstructions(pair *protocol.PairStartResponse) (string, error) {
	code := pair.ManualPairingCode
	if code == "" {
		code = pair.PairingCode
	}
	link, _ := a.PairingLink(code)
	qr, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Pairing code: %s\nPairing link: %s\nExpires at: %s\nScan with the CodexRM app:\n%s", code, link, time.Unix(pair.ExpiresAt, 0).Local().Format(time.RFC3339), qr.ToSmallString(false)), nil
}
