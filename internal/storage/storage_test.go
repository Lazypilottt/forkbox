package storage

import "testing"

func TestReflinkCapable(t *testing.T) {
	for fs, want := range map[string]bool{FSXFS: true, FSBtrfs: true, FSExt4: false, FSTmpfs: false, FSOverlay: false, "": false} {
		if got := ReflinkCapable(fs); got != want {
			t.Errorf("ReflinkCapable(%q) = %v, want %v", fs, got, want)
		}
	}
}
