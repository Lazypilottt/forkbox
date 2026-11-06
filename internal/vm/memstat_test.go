package vm

import "testing"

func TestParseSmapsRollup(t *testing.T) {
	const in = `55d0c6a4c000-7ffd8e5fe000 ---p 00000000 00:00 0                          [rollup]
Rss:              120400 kB
Pss:               35210 kB
Pss_Anon:          20100 kB
Shared_Clean:      90000 kB
Shared_Dirty:          0 kB
Private_Clean:      2400 kB
Private_Dirty:     28000 kB
Anonymous:         28000 kB
`
	m, err := parseSmapsRollup(in)
	if err != nil {
		t.Fatal(err)
	}
	if m.Rss != 120400<<10 || m.Pss != 35210<<10 || m.SharedClean != 90000<<10 || m.PrivateDirty != 28000<<10 || m.Anonymous != 28000<<10 {
		t.Errorf("got %+v", m)
	}
	if _, err := parseSmapsRollup("nothing here"); err == nil {
		t.Error("garbage parsed")
	}
}
