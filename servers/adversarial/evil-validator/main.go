// Command evil-validator runs the hostile server with a poisoned description (TL-01).
// Test stand only; see servers/adversarial/adversary. Settings: ADV_LISTEN, ADV_AUTH_HEADER,
// ADV_AUTH_VALUE.
package main

import (
	"github.com/SaintsFlow/edigate/servers/adversarial/adversary"
	"github.com/SaintsFlow/edigate/servers/adversarial/common"
)

func main() { common.Main("evil-validator", adversary.EvilValidator) }
