package sanitize

import (
	"strings"
	"testing"
)

// BKRef is the raw business reference for the idempotency key: BEG03 of an 850,
// BIG02 of an 810, trimmed at the edges, with case and leading zeros kept. The
// agent sees a note reference when the value fails its format; BKRef still has it.
func TestBKRef(t *testing.T) {
	r := mustBuild(t, withBody(t, "BEG*00*SA*po-0017A**20261018~PO1*1*1*EA*1**VN*A~CTT*1~"))
	if r.BKRef != "po-0017A" || r.BusinessRef != "po-0017A" {
		t.Errorf("BKRef = %q, BusinessRef = %q", r.BKRef, r.BusinessRef)
	}

	// Spaces at the edges are cut from BKRef; the format of the agent view refuses them.
	r = mustBuild(t, withBody(t, "BEG*00*SA*  po-0017A  **20261018~PO1*1*1*EA*1**VN*A~CTT*1~"))
	if r.BKRef != "po-0017A" || !strings.HasPrefix(r.BusinessRef, "$note_") {
		t.Errorf("BKRef = %q, BusinessRef = %q", r.BKRef, r.BusinessRef)
	}

	// A value the agent view refuses (space and brackets) is still the key's raw value.
	r = mustBuild(t, withBody(t, "BEG*00*SA*PO 001 (A)**20261018~PO1*1*1*EA*1**VN*A~CTT*1~"))
	if r.BKRef != "PO 001 (A)" || !strings.HasPrefix(r.BusinessRef, "$note_") {
		t.Errorf("BKRef = %q, BusinessRef = %q: the raw value must survive, the agent sees a note", r.BKRef, r.BusinessRef)
	}

	r = mustBuild(t, setOf(t, "810_full.edi"))
	if r.BKRef == "" || r.BKRef != r.BusinessRef {
		t.Errorf("810 BKRef = %q, BusinessRef = %q", r.BKRef, r.BusinessRef)
	}
}
