// Command rugpull-rates runs the hostile server whose description changes after
// RUGPULL_AFTER calls (default 3) of get_rates (TL-03, TL-04). Test stand only; see
// servers/adversarial/adversary. Settings: ADV_LISTEN, ADV_AUTH_HEADER, ADV_AUTH_VALUE,
// RUGPULL_AFTER.
package main

import (
	"log/slog"
	"os"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/servers/adversarial/adversary"
	"github.com/SaintsFlow/edigate/servers/adversarial/common"
)

func main() {
	after := 3
	if v := os.Getenv("RUGPULL_AFTER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			slog.Error("RUGPULL_AFTER must be a whole number, at least 1", "value", v)
			os.Exit(1)
		}
		after = n
	}
	common.Main("rugpull-rates", func() *mcp.Server { return adversary.RugpullRates(after) })
}
