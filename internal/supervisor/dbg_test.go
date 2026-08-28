package supervisor

import (
	"path/filepath"
	"testing"
)

func TestDbgDerive(t *testing.T) {
	root := t.TempDir()
	makeCheckout(t, filepath.Join(root, "widgets"), "acme/widgets")
	cacheDir := t.TempDir()
	o := materializeOptions(t, cacheDir, root)
	t.Logf("WorkdirRoot=%q", o.WorkdirRoot)
	d := &Supervisor{o: o, hostname: "testhost", cacheDir: cacheDir}
	entry := runEntry("daemon-one", "acme/widgets")
	cfg, sd, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("derived workdir=%q stateDir=%q", cfg.WorkDir, sd)
}