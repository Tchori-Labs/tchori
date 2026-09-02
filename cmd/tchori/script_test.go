package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// updateScripts gates in-place rewriting of the in-archive golden files a
// script compares against with `cmp`. It defaults to false so a plain
// `go test` can never rewrite the expectations it is supposed to be checking;
// refresh them deliberately with
// `go test ./cmd/tchori -run TestScript -update`.
var updateScripts = flag.Bool("update", false, "rewrite the golden files inside cmd/tchori/testdata/script/*.txtar")

// ScriptCmdMain runs the tchori CLI in-process. testscript installs a copy of
// the test binary in $PATH under the name "tchori" and re-execs it for every
// `exec tchori ...` line, dispatching on argv[0]; this is the entry point for
// that dispatch. It is exported because TestMain lives in the external test
// package (cli_test.go), which registers it with testscript.Main.
func ScriptCmdMain() { main() }

// TestScript runs the txtar acceptance scripts in testdata/script against the
// real CLI. Each script becomes a subtest named after its file, minus the
// .txtar suffix.
func TestScript(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata/script",
		// Scripts must say `exec tchori`, so a script can never silently
		// resolve some other tchori that happens to sit on the host PATH.
		RequireExplicitExec: true,
		RequireUniqueNames:  true,
		UpdateScripts:       *updateScripts,
		Setup:               setupScriptEnv,
	})
}

// setupScriptEnv makes each script hermetic. testscript already builds the
// environment from scratch rather than inheriting it, so this only has to
// pin the two escape hatches that remain: a real user home and the ambient
// proxy configuration. HOME and the XDG directories point inside $WORK, and
// the proxies point at a closed port so an accidental network call fails
// closed instead of reaching the internet — the same blackhole-proxy
// convention CI's e2e job uses.
func setupScriptEnv(env *testscript.Env) error {
	home := filepath.Join(env.WorkDir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	env.Setenv("HOME", home)
	env.Setenv("USERPROFILE", home) // windows equivalent of HOME
	env.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	env.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	env.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	env.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	env.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	env.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	env.Setenv("NO_PROXY", "127.0.0.1,localhost")
	return nil
}
