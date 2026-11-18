package supervisor

import (
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// The planner's local `role` is a machine fact, and the supervisor materializes
// each child's config from the machine layer: the role must survive that round
// trip, or a roster entry that starts a planner would come up as a worker and
// drain the backlog instead of serving chat (studio-chat.md §2).
func TestMaterializedConfigCarriesThePlannerRole(t *testing.T) {
	cacheDir := t.TempDir()
	o := materializeOptions(t, cacheDir, "")
	o.BaseCfg = baseWithProject(t, config.Project{Slug: "acme", WorkDir: t.TempDir()})
	o.BaseCfg.Role = config.RolePlanner
	d := &Supervisor{o: o, hostname: "testhost", cacheDir: cacheDir}

	entry := runEntry("planner-one", "acme/widgets")
	cfg, stateDir, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	inst := &Instance{name: entry.Name, entry: &entry, cfg: cfg, stateDir: stateDir}
	path, err := d.materializeConfig(inst)
	if err != nil {
		t.Fatalf("materializeConfig: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if got := loaded.EffectiveRole(); got != config.RolePlanner {
		t.Fatalf("materialized role = %q, want planner - the child would run the backlog drain", got)
	}
	// And a worker base must not gain a key it never set: the identity of every
	// existing fleet child stays exactly what it was.
	o.BaseCfg.Role = ""
	cfg, stateDir, err = d.buildConfig(&entry)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	inst = &Instance{name: entry.Name, entry: &entry, cfg: cfg, stateDir: stateDir}
	if path, err = d.materializeConfig(inst); err != nil {
		t.Fatalf("materializeConfig: %v", err)
	}
	loaded, err = config.Load(path)
	if err != nil {
		t.Fatalf("generated worker config does not load: %v", err)
	}
	if got := loaded.EffectiveRole(); got != config.RoleWorker {
		t.Fatalf("materialized role = %q, want the implicit worker", got)
	}
}
