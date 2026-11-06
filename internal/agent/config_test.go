package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadGuestConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadGuestConfig(filepath.Join(dir, "missing.json"))
	if err != nil || len(c.Env) != 0 || c.Cwd != "" {
		t.Errorf("missing file: %+v, %v", c, err)
	}
	p := filepath.Join(dir, "agent.json")
	os.WriteFile(p, []byte(`{"env":["PATH=/usr/local/bin:/usr/bin","LANG=C"],"cwd":"/work"}`), 0o644)
	c, err = LoadGuestConfig(p)
	if err != nil || !reflect.DeepEqual(c.Env, []string{"PATH=/usr/local/bin:/usr/bin", "LANG=C"}) || c.Cwd != "/work" {
		t.Errorf("got %+v, %v", c, err)
	}
	os.WriteFile(p, []byte(`{not json`), 0o644)
	if _, err := LoadGuestConfig(p); err == nil {
		t.Error("bad json accepted")
	}
}
