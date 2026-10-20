package mode

import "testing"

func TestParse(t *testing.T) {
	for _, m := range All {
		if got, err := Parse(string(m)); err != nil || got != m {
			t.Errorf("Parse(%q) = %q, %v", m, got, err)
		}
	}
	if got, err := Parse(""); err != nil || got != Full {
		t.Errorf("an empty value must mean full: %q, %v", got, err)
	}
	for _, bad := range []string{"bogus", "FULL", "plain ", "no_policy"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) must fail", bad)
		}
	}
}

// The questions the modes answer, as the table of the protocol says.
func TestAnswers(t *testing.T) {
	cases := []struct {
		m                                 Mode
		open, rawText, noteTool, modelVal bool
	}{
		{Full, false, false, true, false},
		{Plain, true, true, false, true},
		{ReqHash, true, true, false, true},
		{NoSanitize, false, true, false, false},
		{NoPolicy, false, false, true, false},
		{NoBizKey, false, false, true, false},
		{NoContracts, false, false, true, true},
	}
	for _, c := range cases {
		if c.m.Open() != c.open || c.m.RawText() != c.rawText || c.m.NoteTool() != c.noteTool || c.m.ModelValues() != c.modelVal {
			t.Errorf("%s: open %v raw %v note %v model %v", c.m, c.m.Open(), c.m.RawText(), c.m.NoteTool(), c.m.ModelValues())
		}
	}
}
