package pz

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeTree creates a set of directories below root.
func makeTree(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
}

// TestDetectGameRootLayouts pins the install layouts PZAdmin must recognise.
//
// The two-levels-down case is the regression that mattered: real installs sit
// at <server>/projectzomboid/data/media/scripts, mods were found there, and
// the vanilla items were not.
func TestDetectGameRootLayouts(t *testing.T) {
	cases := []struct {
		name string
		dirs []string
		want string // relative to base, "" means not found
	}{
		{
			name: "install at base",
			dirs: []string{"media/scripts"},
			want: ".",
		},
		{
			name: "install one level down",
			dirs: []string{"ZomboidDedicatedServer/media/scripts"},
			want: "ZomboidDedicatedServer",
		},
		{
			name: "install two levels down under projectzomboid/data",
			dirs: []string{"projectzomboid/data/media/scripts", "projectzomboid/data/Server"},
			want: "projectzomboid/data",
		},
		{
			name: "install under projectzomboid/config",
			dirs: []string{"projectzomboid/config/media/scripts"},
			want: "projectzomboid/config",
		},
		{
			name: "install under a Steam library",
			dirs: []string{"steamapps/common/Project Zomboid Dedicated Server/media/scripts"},
			want: "steamapps/common/Project Zomboid Dedicated Server",
		},
		{
			name: "install under a nested Steam library",
			dirs: []string{"data/Steam/steamapps/common/PZ Server/media/scripts"},
			want: "data/Steam/steamapps/common/PZ Server",
		},
		{
			name: "data only, no install present",
			dirs: []string{"Server", "Saves", "mods"},
			want: "",
		},
		{
			name: "media without scripts is not an install",
			dirs: []string{"projectzomboid/data/media/lua"},
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			makeTree(t, base, c.dirs...)

			got := DetectGameRoot(base)
			want := ""
			if c.want != "" {
				want = filepath.Clean(filepath.Join(base, filepath.FromSlash(c.want)))
			}
			if got != want {
				t.Fatalf("DetectGameRoot = %q, want %q", got, want)
			}
		})
	}
}

// TestDetectGameRootPrefersShallowest guards the ordering: a server directory
// that is itself an install must not be passed over for a nested copy.
func TestDetectGameRootPrefersShallowest(t *testing.T) {
	base := t.TempDir()
	makeTree(t, base,
		"media/scripts",
		"projectzomboid/data/media/scripts",
		"steamapps/common/Project Zomboid Dedicated Server/media/scripts",
	)
	if got, want := DetectGameRoot(base), filepath.Clean(base); got != want {
		t.Fatalf("DetectGameRoot = %q, want the base itself %q", got, want)
	}
}

// TestDetectGameRootIsSetByLayout checks the wiring, not just the helper: a
// Layout built from a realistic tree must carry GameDir.
func TestDetectGameRootIsSetByLayout(t *testing.T) {
	base := t.TempDir()
	makeTree(t, base,
		"projectzomboid/data/media/scripts",
		"projectzomboid/data/Server",
		"projectzomboid/data/Saves",
		"projectzomboid/data/steamapps/workshop/content/108600",
	)
	if err := os.WriteFile(filepath.Join(base, "projectzomboid", "data", "Server", "test.ini"),
		[]byte("Mods=\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := Detect(base)
	if !l.Valid() {
		t.Fatal("layout should be valid: it has a Server directory with an ini")
	}
	want := filepath.Join(base, "projectzomboid", "data")
	if l.GameDir != want {
		t.Fatalf("GameDir = %q, want %q", l.GameDir, want)
	}
	if len(l.ModDirs) == 0 {
		t.Fatal("mod directories should still be found")
	}
}

// TestScanFindsVanillaInGeneratedSubfolders reproduces the Build 42 layout,
// where items live several directories below media/scripts rather than in it.
func TestScanFindsVanillaInGeneratedSubfolders(t *testing.T) {
	base := t.TempDir()
	items := filepath.Join(base, "projectzomboid", "data", "media", "scripts", "generated", "items")
	if err := os.MkdirAll(items, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `module Base
{
    item Axe
    {
        DisplayName = Axe,
        DisplayCategory = ToolWeapon,
        Type = Weapon,
    }
    item Acorn
    {
        DisplayName = Acorn,
        DisplayCategory = Food,
        Type = Food,
    }
}
`
	if err := os.WriteFile(filepath.Join(items, "weapon.txt"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}

	root := DetectGameRoot(base)
	if root == "" {
		t.Fatal("game root not detected")
	}
	cat := NewScriptScanner(time.Minute).Scan(root, nil)
	if len(cat.Items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(cat.Items), cat.Items)
	}
	for _, e := range cat.Items {
		if e.Source != "vanilla" {
			t.Fatalf("item %s has source %q, want vanilla", e.ID, e.Source)
		}
	}
}
