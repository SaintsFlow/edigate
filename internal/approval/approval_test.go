package approval

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The life of a request is 30 minutes divided by the time scale, but not below 30 seconds.
func TestTTL(t *testing.T) {
	for scale, want := range map[float64]time.Duration{
		0: 30 * time.Minute, 1: 30 * time.Minute, 60: 30 * time.Second, 1e6: 30 * time.Second, 2: 15 * time.Minute,
	} {
		if got := TTL(scale); got != want {
			t.Errorf("TTL(%v) = %v, want %v", scale, got, want)
		}
	}
}

// The view holds the tool, the tier, the rule names and the arguments, and an empty
// rule list is an empty array, not null.
func TestView(t *testing.T) {
	raw, err := View("erp.create_sales_order", 1, nil, map[string]string{"total": "1.00"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); !strings.Contains(got, `"rules":[]`) || !strings.Contains(got, `"tool":"erp.create_sales_order"`) {
		t.Errorf("view = %s", got)
	}
	var v struct {
		Tier int `json:"tier"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Tier != 1 {
		t.Errorf("tier = %d %v", v.Tier, err)
	}
}
