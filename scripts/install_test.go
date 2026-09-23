package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerRefreshesServiceThroughInstalledBinary(t *testing.T) {
	temp := t.TempDir()
	fakeBin := filepath.Join(temp, "bin")
	installDir := filepath.Join(temp, "installed")
	for _, dir := range []string{fakeBin, installDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	scripts := map[string]string{
		"curl": "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then shift; touch \"$1\"; exit; fi; shift; done\nexit 1\n",
		"tar": `#!/bin/sh
while [ "$#" -gt 0 ]; do
 if [ "$1" = -C ]; then shift; dest="$1"; fi
 shift
done
mkdir -p "$dest/syna-test-linux-amd64"
cat > "$dest/syna-test-linux-amd64/syna" <<'BIN'
#!/bin/sh
if [ "$1" = help ]; then
 if [ "${LEGACY_BINARY:-0}" = 0 ]; then echo "syna service refresh"; fi
 exit 0
fi
printf '%s\n' "$0 $*" >> "$TEST_CALLS"
if [ "$1" = service ]; then exit "${REFRESH_EXIT:-0}"; fi
exit 0
BIN
chmod +x "$dest/syna-test-linux-amd64/syna"
`,
		"systemctl": "#!/bin/sh\nprintf '%s\\n' \"systemctl $*\" >> \"$TEST_CALLS\"\nexit 0\n",
		"uname":     "#!/bin/sh\nif [ \"$1\" = -s ]; then echo Linux; else echo x86_64; fi\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"success", "failure", "legacy"} {
		failure := name == "failure"
		t.Run(name, func(t *testing.T) {
			calls := filepath.Join(temp, name+"-calls")
			cmd := exec.Command("sh", "install.sh")
			cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "INSTALL_DIR="+installDir, "SYNA_VERSION=test", "TEST_CALLS="+calls)
			if failure {
				cmd.Env = append(cmd.Env, "REFRESH_EXIT=1")
			}
			if name == "legacy" {
				cmd.Env = append(cmd.Env, "LEGACY_BINARY=1")
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("installer: %v\n%s", err, output)
			}
			got, err := os.ReadFile(calls)
			want := "systemctl --user cat syna.service\n" + installDir + "/syna service refresh\n" + installDir + "/syna version\n"
			if name == "legacy" {
				want = "systemctl --user cat syna.service\nsystemctl --user daemon-reload\nsystemctl --user restart syna.service\n" + installDir + "/syna version\n"
			}
			if err != nil || string(got) != want {
				t.Fatalf("calls=%q err=%v, want %q", got, err, want)
			}
			if failure && !strings.Contains(string(output), "could not refresh the user service") {
				t.Fatalf("refresh failure hidden: %s", output)
			}
		})
	}
}
