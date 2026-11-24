package vm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestNoAMXCPUConfig(t *testing.T) {
	var c struct {
		CPUID []struct {
			Leaf, Subleaf string
			Flags         int
			Modifiers     []struct{ Register, Bitmap string }
		} `json:"cpuid_modifiers"`
	}
	if err := json.Unmarshal(NoAMXCPUConfig, &c); err != nil {
		t.Fatal(err)
	}
	cleared := map[string][]int{}
	for _, l := range c.CPUID {
		for _, m := range l.Modifiers {
			b := strings.TrimPrefix(m.Bitmap, "0b")
			if len(b) != 32 || strings.Trim(b, "x0") != "" {
				t.Fatalf("%s.%s %s: bitmap %q", l.Leaf, l.Subleaf, m.Register, m.Bitmap)
			}
			for i, ch := range b {
				if ch == '0' {
					key := l.Leaf + "." + l.Subleaf + "." + m.Register
					cleared[key] = append(cleared[key], 31-i)
				}
			}
		}
	}
	want := map[string]string{"0x7.0x0.edx": "[25 24 22]", "0xd.0x0.eax": "[18 17]", "0xd.0x1.eax": "[4]"}
	for k, v := range want {
		if got := fmt.Sprint(cleared[k]); got != v {
			t.Errorf("%s clears bits %s, want %s", k, got, v)
		}
	}
}
