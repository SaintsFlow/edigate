// Command shadow-carrier runs the hostile server with a tool that shadows a tool of the
// gateway (TL-02). Test stand only; see servers/adversarial/adversary. Settings: ADV_LISTEN,
// ADV_AUTH_HEADER, ADV_AUTH_VALUE.
package main

import (
	"github.com/SaintsFlow/edigate/servers/adversarial/adversary"
	"github.com/SaintsFlow/edigate/servers/adversarial/common"
)

func main() { common.Main("shadow-carrier", adversary.ShadowCarrier) }
